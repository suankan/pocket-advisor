# Retrieval Design

This document is the design authority for Pocket Advisor's read path. It describes the retrieval package that is implemented now and the deliberately intended service boundary. [Ingestion design](ingestion-design.md) owns the indexed data and write path, [workspace isolation](workspace-isolation.md) owns storage and credential boundaries, [API server design](api-server-design.md) owns the future network interface, and [generation design](generation-design.md) owns the future answer service.

## Status

The transport-independent Go retrieval package and host CLI query mode are current. The workspace-bound stdio MCP server and authenticated Streamable HTTP MCP adapter are described in [MCP server design](mcp.md). A general HTTP user API and an in-repository answer-generation service remain target state.

## Principles

- Retrieve source text only. The index contains extracted or normalized source text, not model-produced summaries.
- Run dense and lexical search together because they cover different failure modes.
- Preserve byte provenance through document IDs, chunk byte ranges, source hashes, and Tier 1 URIs.
- Enforce workspace scope through separate databases and credentials, then assert the connected database's contents at process startup.
- Return quality degradations as structured warnings instead of hiding them in logs.
- Keep the retrieval package independent of CLI, MCP, and HTTP transport types.

## Current query flow

`internal/retrieval.Service.Query` executes the following stages:

1. Validate the question and resolve request overrides.
2. Ask the configured local chat-completions model to split the question into at most four independent queries. If decomposition is disabled or fails, use the original question.
3. For every sub-query, request an embedding and run dense search, lexical search, and reciprocal-rank fusion in one PostgreSQL round trip.
4. Merge the candidate groups into a bounded rerank pool, reserving space for each sub-query and for dense-only candidates.
5. Rerank the pool once against the original question. If reranking is disabled or unavailable, preserve fused order.
6. Apply the relevance floor, keep at most one hit per document, and cap results from one email thread.
7. Load matched documents and related parent, attachment, and same-thread documents, then fill one shared answer-context budget breadth-first.
8. Return content packets, sub-queries, budget use, and structured warnings.

The service owns warm model clients but no request state. PostgreSQL owns indexed and lineage state.

## Candidate generation

### Dense leg

The dense leg performs cosine-distance search over the HNSW index on `document_chunks.embedding`. It filters on the embed-model namespace and takes up to 50 candidates by default. The query vector comes from the same configured embedding model that names the indexed namespace.

`internal/storage/postgres.ApplySchema` rejects an existing vector column whose dimension differs from the configured embedding dimension. Ingestion and listener startup also verify that stored chunks do not use a different model or dimension. Retrieval startup currently asserts workspace scope but does not repeat the schema-metadata check; it filters candidates to the query embedder's model namespace. A different embedding model therefore requires the explicit re-embedding workflow described in [ingestion design](ingestion-design.md); changing configuration alone is not an upgrade path.

### Lexical leg

The lexical leg uses the `pg_textsearch` BM25 index `chunks_bm25_idx` and takes up to 50 candidates by default. `to_bm25query` tokenizes the raw sub-query against the index's `simple` text configuration. The query filters on the same embed-model namespace as dense search so an index containing two embedding namespaces cannot surface the same source chunk twice.

### Reciprocal-rank fusion

Dense and lexical candidates are combined with a full outer join. Each candidate receives:

```text
1 / (rrf_k + dense_rank) + 1 / (rrf_k + lexical_rank)
```

A missing leg contributes zero. The default `rrf_k` is 60. Fusion retains candidates found by only one leg, which is necessary for exact identifiers, paraphrases, and cross-language queries.

The fusion query has no `workspace_id` predicate. Every workspace has a separate database, so such a predicate would normally match every row and could conceal contamination. `AssertScope` instead inspects distinct workspace IDs at process startup and refuses to serve a database containing a different or multiple workspace scopes.

### Query decomposition

Decomposition is enabled by default and produces no more than four non-empty, deduplicated sub-queries. It uses the local model configured under `infra.llm`; that model is query preparation only and never writes an answer. Requests may disable decomposition. A model failure is non-fatal and adds `decomposition_unavailable` while searching the original question.

## Pooling and reranking

The default rerank window contains 24 unique chunks. Filling it only by fused score can remove candidates that are structurally disadvantaged because one search leg could not match or another sub-query dominated the pool. The implementation therefore reserves, by default, up to four candidates per sub-query and up to six dense-only candidates before filling remaining slots by fused score. It reports `pool_floor_applied` when reservations were used and the pool filled to its limit.

The configured reranker scores each whole candidate chunk against the original question. A reranker error degrades to fused order and reports `reranker_unavailable`. In fallback order the implementation leaves scores at zero; with the current zero relevance floor, an outage therefore does not turn every query into an empty result.

The model choices in `config.yaml` are configurable because endpoints vary by installation, but retrieval quality and latency are model-dependent. Operators should treat a model change as a calibration change and verify it against representative queries before relying on the results.

## Selection and evidence packets

The compiled defaults are 15 returned matches, a reranker relevance floor of `0.0`, and at most three matches from one non-empty email thread. Selection proceeds in this order:

1. remove candidates below the relevance floor;
2. keep only the highest-ranked chunk for each document;
3. enforce the per-thread cap; and
4. stop at the requested result count.

An empty `thread_id` denotes a standalone document and is not capped as a shared conversation. A below-floor candidate is never restored merely to fill the requested count.

Each returned packet contains the matched document's metadata and source text, the chunk ID and UTF-8 byte range, its reranker score, the search leg or legs that found it, a snippet, source hash, and Tier 1 URI. The transport-independent result and CLI JSON use `start_byte` and `end_byte`; MCP additionally carries `offset_unit: utf8_bytes`. Related documents are labeled using stored facts:

- `parent` for the matched document's recorded parent;
- `attachment` for a recorded child document; and
- `same-thread` for another document with the same non-empty thread ID.

Chronological adjacency is never represented as a reply edge.

All packets share a default 120,000 UTF-8 byte context budget. The budgeter charges `len(text)`, exposes `bytes_used` and `bytes_allowed` in the transport-independent result, and labels the unit explicitly in MCP. It first offers each packet its matched document, then related documents, so a large conversation cannot consume the allowance before other primary matches are considered. Text that does not fit is omitted and produces `budget_truncated`; the document metadata and relationship remain available.

## Results and degradation signals

`retrieval.Result` contains the original question, effective sub-queries, evidence packets, warnings, and used and allowed UTF-8 byte counts. Empty packet and warning collections are serialized as empty arrays.

Current warnings are:

| Warning | Meaning |
| --- | --- |
| `dense_leg_underfill` | Dense search yielded fewer candidates than configured while the rerank pool also remained underfilled. |
| `lexical_query_empty` | A sub-query contained no text for BM25. |
| `decomposition_unavailable` | Decomposition failed or returned no usable query; the original question was used. |
| `pool_floor_applied` | Candidate reservations were used and the rerank pool filled to its limit. |
| `reranker_unavailable` | Reranking failed and fused order was returned. |
| `relevance_floor_applied` | Below-floor candidates caused fewer than the requested results to be selected. |
| `thread_capped` | A conversation produced more selected documents than the per-thread limit. |
| `budget_truncated` | Some document text did not fit the shared UTF-8 byte allowance. |

Embedding, SQL, lineage-loading, and packet-building errors fail the query. Decomposition and reranking failures have defined fallbacks because the system can still produce a useful, explicitly degraded result.

The current process emits structured logs and includes warning codes in CLI and MCP results. Retrieval-specific Prometheus counters and latency histograms are target state; they are not yet registered by `internal/retrieval`.

## Current interfaces

### CLI

`./bin/pocket-advisor --query '<question>' --workspace-id <id>` builds a retrieval service for the selected workspace, asserts database scope, runs the query, and prints packet metadata and text. `--top-k`, `--json`, `--no-rerank`, and `--no-decompose` override request behavior or output.

### MCP transports

Both the stdio and authenticated Streamable HTTP MCP transports are described in [MCP server design](mcp.md). They expose the same tool contract, evidence interface, citation system, pagination, and response bounds over the retrieval package. The retrieval package remains transport-independent; both MCP adapters are thin boundaries over `internal/retrieval` and the shared `internal/mcp.QueryTool`.

## Evaluation

`pocket-advisor --evaluate` runs a private workspace case set through the same retrieval service and reports document recall, reciprocal rank, nDCG, forbidden hits, warnings, candidate yields, and stage timing. A case passes only when it satisfies its explicit contract: an expected-empty case has no packets, a regular case returns at least one expected document, a case with `require_all_expected_documents` returns every expected document, and no forbidden document is returned. Aggregate ranking thresholds remain the run-level quality gate; an optional `--eval-hnsw` run also gates approximate-search recall.

The operator maintains the version 3 case set as a curated private golden suite outside version control. Each `expected_documents` entry contains a PostgreSQL document UUID and relevance grade; `require_all_expected_documents` makes every listed document mandatory, and `forbidden_document_ids` contains PostgreSQL document UUIDs. The evaluator never generates cases from workspace content and never resolves evaluation identity through filenames or source hashes. Each report records a SHA-256 identity of its case-set contents, and results are comparable only when that case-set identity is unchanged.

## Target service boundary

The broader user API remains one long-running retrieval workload per workspace, reached only through the authenticated API gateway described in [API server design](api-server-design.md). Future non-MCP HTTP adapters translate transport requests into the same `retrieval.Request` and return the same result semantics. They must not accept a caller-supplied workspace that bypasses the gateway's authenticated route.

The future generation service described in [generation design](generation-design.md) is a separate consumer of evidence packets. Retrieval remains usable without it and does not gain answer-model credentials.

Target observability should expose per-stage latency, candidate yields by leg, warning counts, selected-result counts, and context-budget utilization without logging questions or source content.

## Verification

Use the repository commands in [README §10](../README.md#10-verification). Retrieval behavior is covered by unit tests under `internal/retrieval`, storage integration tests, and the manual query and supported-client smoke checks in the handbook. MCP-specific tests are described in [MCP server design](mcp.md#testing).

## Open decisions

- Decide whether long-lived retrieval traffic requires an explicit HNSW iterative-scan policy or other recall guard as the index grows.
- Decide whether retrieval startup must verify its configured embedding model and dimension against `schema_metadata`, matching ingestion's fail-fast check.
- Calibrate representative category coverage and acceptance thresholds for model or tuning changes as corpora and models change.
- Choose metrics buckets and labels that diagnose quality and latency without exposing workspace identifiers or query text.
- Decide whether multi-turn question state belongs in the future generation service or solely in its client.
