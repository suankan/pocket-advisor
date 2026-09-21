# Retrieval Design

This document is the design authority for Pocket Advisor's read path. It describes the retrieval package that is implemented now and the deliberately intended service boundary. [Ingestion design](ingestion-design.md) owns the indexed data and write path, [workspace isolation](workspace-isolation.md) owns storage and credential boundaries, [API server design](api-server-design.md) owns the future network interface, and [generation design](generation-design.md) owns the future answer service.

## Status

The transport-independent Go retrieval package, exact mailbox browse package, and host CLI query mode are current. The workspace-bound stdio MCP server and authenticated Streamable HTTP MCP adapter are described in [MCP server design](mcp.md). A general HTTP user API and an in-repository answer-generation service remain target state.

## Principles

- Retrieve source text only. The index contains extracted or normalized source text, not model-produced summaries.
- Run dense and lexical search together because they cover different failure modes.
- Preserve byte provenance through document IDs, chunk byte ranges, source hashes, and original-source references.
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
8. When topic-graph expansion is enabled, offer whatever remains of that same budget to the derived graph supplement.
9. Return content packets, sub-queries, budget use, optional graph expansion, and structured warnings.

The service owns warm model clients but no request state. PostgreSQL owns indexed and lineage state.


## Exact email browse

`internal/mailbox` is a transport-independent, fixed-workspace read service over durable email metadata. It lists messages by exact normalized sender or recipient (To, Cc, or Bcc), parsed-date bounds, and owner-relative inbound, outbound, or either direction. Newest-first is the default; oldest-first is its exact reverse. Pages default to 25 and are bounded at 200; conversation collapse returns one matched message with a bounded summary.

Opaque cursors bind order and all filters to an `ingested_at` snapshot and a keyset position, so later ingestion cannot shift, repeat, or hide a page series. Date filters exclude undated messages and report that omission. A server-issued message or conversation reference fetches a deduplicated email-only conversation in stable chronological order, with relationship methods, bounded participants, warnings, and omission counts.

Awaiting-reply candidates require configured owner identities and exact-reference conversations. A candidate is the latest relevant human inbound message with no later human owner-authored exact-reply descendant. Participant and date bounds select the inbound candidate but never hide later evidence; automated and third-party events remain labelled evidence. Subject grouping and similarity never prove a reply or close a candidate.

The MCP tools in [MCP server design](mcp.md) adapt this service; semantic retrieval may remain independent of mailbox browse.

## Candidate generation

### Dense leg

The dense leg performs cosine-distance search over the HNSW index on shared `chunks.embedding`, joins the resulting passage to its `document_chunks` placements, and takes up to 50 candidates by default. The query vector comes from the same configured embedding model that names the indexed namespace. A placement remains the retrieval candidate and citation identity, so one shared passage can still be cited in the document where it was matched.

`internal/storage/postgres.ApplySchema` rejects an existing vector column whose dimension differs from the configured embedding dimension. Ingestion and listener startup also verify that stored chunks do not use a different model or dimension. Retrieval startup currently asserts workspace scope but does not repeat the schema-metadata check; it filters candidates to the query embedder's model namespace. A different embedding model therefore requires the explicit re-embedding workflow described in [ingestion design](ingestion-design.md); changing configuration alone is not an upgrade path.

### Lexical leg

The lexical leg uses the `pg_textsearch` BM25 index `chunks_bm25_idx` on shared `chunks.chunk_text`, joins matching passages to their `document_chunks` placements, and takes up to 50 candidates by default. `to_bm25query` tokenizes the raw sub-query against the index's `simple` text configuration. The query filters on the same embed-model namespace as dense search so an index containing two embedding namespaces cannot surface the same source chunk twice.

### Reciprocal-rank fusion

Dense and lexical candidates are combined with a full outer join. Each candidate receives:

```text
1 / (rrf_k + dense_rank) + 1 / (rrf_k + lexical_rank)
```

A missing leg contributes zero. The default `rrf_k` is 60. Fusion retains candidates found by only one leg, which is necessary for exact identifiers, paraphrases, and cross-language queries.

The fusion query has no `workspace_id` predicate, and no table it reads carries that column at all: every workspace has a separate database, so a per-row workspace identity would always equal the same one value and could never disambiguate anything. `AssertScope` instead checks the single workspace id `schema_metadata` records for the whole database, once, at process startup, and refuses to serve a database recorded for a different workspace.

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
3. keep only the highest-ranked candidate for each exact whitespace-normalised passage text across documents;
4. enforce the per-thread cap; and
5. stop at the requested result count.

An empty `thread_id` denotes a standalone document and is not capped as a shared conversation. A below-floor candidate is never restored merely to fill the requested count.

Each returned packet contains document metadata, normalized source text, matched placement identity and UTF-8 byte range, ranking score, search legs, source hash, canonical extraction hash/version, and `source:<doc_id>`. Returned candidate text is sliced from the owning normalized document, so whitespace-deduplicated shared passages cannot substitute another document's spelling or offsets. `slice_sha256` identifies the exact placement bytes; the shortened snippet is display text.

`source_locations` lists up to 128 recorded direct or container routes, ordered by observed availability, shortest recipe, and path. Each has an opaque root ID, relative/display path, versioned selectors from original to child, `last_observed` or `missing` status, and observation time. The location query walks all recorded parents with cycle/depth bounds. `source_path` (`workspace_relative_path` in MCP) and `container_source_path` (`container_workspace_relative_path` in MCP) are compatibility display fields derived from the preferred route. Either may be absent if no route is known; both direct and contained routes may exist in the structured list.

Ordinary retrieval does not reopen originals. Locations state inventory observations, not current file availability. Cached text and MCP continuation snapshots remain usable after a source changes or goes offline; the source hash and extraction hash identify the cited revision. The local CLI resolves configured roots for the operator's absolute path. MCP returns relative metadata and never grants arbitrary filesystem access.

Related documents are labeled using stored facts:

- `parent` for the matched document's recorded parent;
- `attachment` for a recorded child document; and
- `same-thread` for another document with the same non-empty thread ID.

Chronological adjacency is never represented as a reply edge.

All packets share a default 120,000 UTF-8 byte context budget. The budgeter charges `len(text)`, exposes `bytes_used` and `bytes_allowed` in the transport-independent result, and labels the unit explicitly in MCP. It first offers each packet its matched document, then related documents, so a large conversation cannot consume the allowance before other primary matches are considered. Text that does not fit is omitted and produces `budget_truncated`; the document metadata and relationship remain available.

## Optional topic-graph expansion

Graph expansion is a supplement to ordinary packets, not a candidate source. It is off by default (`infra.query.topic_graph_expansion_enabled`) so an ordinary query never reads derived graph data, and it runs only after packet construction, so ranked packets and their lineage context always consume the shared allowance first and can never be displaced or reordered by it.

When enabled and a query selected at least one hit, the service derives seed mention references from the one `ACTIVE` graph version, admitting only mentions whose validated spans overlap a selected hit and that carry at least one supported edge. It then walks the timeline under the configured backward and forward depth, node count and byte ceiling — the byte ceiling further reduced to whatever the shared budget has left — with a two-second latency bound.

The result carries the graph version, snapshot time, nodes, derived relations, warnings and an omitted-node count. A node is represented only by its canonical document and validated byte spans, each with the document's normalized-text hash and the slice hash: display labels are deliberately absent, so nothing derived from a model reaches the caller as a fact. A disabled gate, an absent graph, no seeds, an exhausted allowance or a failed walk are all visible degradations; none of them fails a query that ordinary retrieval already answered.

## Results and degradation signals

`retrieval.Result` contains the original question, effective sub-queries, evidence packets, warnings, used and allowed UTF-8 byte counts, and the optional `graph_expansion` object described above, which is omitted entirely when expansion is disabled or produced nothing. Empty packet and warning collections are serialized as empty arrays.

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
| `topic_graph_unavailable` | Graph expansion was enabled but its seeds or walk could not be produced; ordinary packets are unaffected. |

An enabled graph walk may additionally surface its own bounded-traversal warnings — `topic_timeline_node_limit`, `topic_timeline_byte_limit`, `topic_timeline_depth_limit`, `topic_timeline_edge_limit`, `topic_timeline_cycle_guard` and `topic_timeline_invalid_evidence` — on the result alongside the graph expansion that produced them.

Embedding, SQL, lineage-loading, and packet-building errors fail the query. Decomposition and reranking failures have defined fallbacks because the system can still produce a useful, explicitly degraded result.

The current process emits structured logs and includes warning codes in CLI and MCP results. Retrieval-specific Prometheus counters and latency histograms are target state; they are not yet registered by `internal/retrieval`.

## Current interfaces

### CLI

`./bin/pocket-advisor --query '<question>' --workspace-id <id>` builds a retrieval service for the selected workspace, asserts database scope, runs the query, and prints packet metadata and text. `--top-k`, `--json`, `--no-rerank`, and `--no-decompose` override request behavior or output.

### MCP transports

Both the stdio and authenticated Streamable HTTP MCP transports are described in [MCP server design](mcp.md). They expose the same tool contract, evidence interface, citation system, pagination, and response bounds over the retrieval package. The retrieval package remains transport-independent; both MCP adapters are thin boundaries over `internal/retrieval` and the shared `internal/mcp.QueryTool`.

## Evaluation

`pocket-advisor --evaluate` runs a private workspace case set through the same retrieval service and reports document recall, delivered recall, reciprocal rank over both, nDCG, the position of the first expected document and of the first delivered one, warnings, candidate yields, and stage timing. A case passes only when it satisfies its explicit contract: an expected-empty case has no packets, a regular case returns at least one expected document, a case with `require_all_expected_documents` returns every expected document, and any expected document found is returned with its text. Aggregate ranking thresholds remain the run-level quality gate; an optional `--eval-hnsw` run also gates approximate-search recall.

The operator maintains the version 6 case set as a curated private golden suite outside version control. A case is an identifier, a question, and the documents that answer it: each `expected_documents` entry carries a `match.raw_sha256` and a relevance grade, and nothing else is recorded. Labels classifying a question's kind were removed with the answer spans — self-declared, unvalidated, and prone to drifting out of step whenever a question is edited, they were a second thing to maintain that no measurement depended on. Content hashes rather than document UUIDs, because a UUID is minted per database — rebuilding a workspace from the same original files renumbers every document and silently invalidates the whole suite, which reads as a total quality collapse rather than as the setup error it is. `raw_sha256` is the key `CreateStub` already upserts on, so "the same bytes are the same document" is enforced by the schema rather than assumed by the evaluator. The trade is deliberate: a UUID survives a document's bytes changing and nothing else, a content hash survives everything except that, and re-ingestion is far more common than re-export.

A case is a question paired with the documents that answer it, and nothing more. It does not restate the answer: the document already holds it, and a copy in the case file is a second source of truth that goes stale the moment a PDF is re-extracted under a newer OCR build. What a caller can do with a result is established by delivering the document, not by asserting its wording.

Delivery is therefore scored separately from retrieval. `DeliveredRecallAtK` counts only expected documents whose text the shared budget actually admitted, and the budget admits a document's text whole or not at all, so a delivered document carries everything it says. A case fails when an expected document is found and its text is not returned: recall reports a perfect result while the caller receives a title and an empty body, unable to answer. The gap between document recall and delivered recall is exactly the budget dropping evidence retrieval had already located.

Depth is measured, never asserted. Each case reports where the first expected document landed and where the first grounded answer landed, and the run reports the reciprocals averaged across the suite; no case declares a required position. A per-case rank bound would have to come from somewhere, and the only available source is what the system currently achieves — which pins present behaviour and calls it a specification. The same objection retired an earlier forbidden-document mechanism: a blocklist assembled from observed output enumerates one of the many near-misses a corpus contains, so it catches the wrong answer already seen and generalises to nothing. Depth belongs in the trend across runs, read against the recorded case-set identity, model names and commit.

Cases are authored against the corpus rather than against retrieval output. `--corpus-search` finds documents whose canonical text contains exact wording, in one whitespace-collapsed full scan that never consults the ranker: an expected answer taken from ranked output makes the case agree with current behaviour by construction and unable to fail for the ranker missing a document, which is the failure a golden set most needs to catch. `--eval-readiness` then confirms every expected document still resolves against the workspace before a run is attempted.

Every complete run is therefore written to `workspaces/evaluation/<workspace>/runs/`, named by its run id so the directory sorts chronologically, and `--eval-report` redirects that single write rather than enabling a second one. A run reports how it moved against the most recent earlier run over the same case-set digest: the aggregate deltas, and the case ids that newly pass or newly fail — an equal number of fixes and regressions leaves every mean looking calm, so the lists are what make that visible. Runs narrowed by `--eval-filter-ids` are excluded from the series, because a summary over three hand-picked cases is not comparable with a summary over the suite.

Every document reference resolves once, before the first query. An unresolved reference fails the run as a setup error naming the affected cases, never as a scored result, and `--eval-readiness` performs the same resolution so an unusable suite is visible without running anything. The evaluator never generates cases from workspace content. Each report records a SHA-256 identity of its case-set contents, and results are comparable only when that case-set identity is unchanged.
When a workspace has an active topic graph, the same evaluation run also emits aggregate graph coverage, closed relation-type and warning counts, confidence distributions, and bounded timeline validity, budget, and omission measurements. The graph portion contains no source content, labels, graph references, document identifiers, paths, or workspace identity. An optional `topic_graph` threshold object can make its coverage, confidence, timeline validity, omission, and warning limits mandatory; a mandatory failure makes `--evaluate` fail without changing graph lifecycle state.

## Target service boundary

The broader user API remains one long-running retrieval workload per workspace, reached only through the authenticated API gateway described in [API server design](api-server-design.md). Future non-MCP HTTP adapters translate transport requests into the same `retrieval.Request` and return the same result semantics. They must not accept a caller-supplied workspace that bypasses the gateway's authenticated route.

The future generation service described in [generation design](generation-design.md) is a separate consumer of evidence packets. Retrieval remains usable without it and does not gain answer-model credentials.

Target observability should expose per-stage latency, candidate yields by leg, warning counts, selected-result counts, and context-budget utilization without logging questions or source content.

## Verification

Use the repository commands in [README §10](../README.md#10-verification). Retrieval behavior is covered by unit tests under `internal/retrieval`, storage integration tests, and the manual query and supported-client smoke checks in the handbook. MCP-specific tests are described in [MCP server design](mcp.md#testing).

## Open decisions

- Decide whether long-lived retrieval traffic requires an explicit HNSW iterative-scan policy or other recall guard as the index grows.
- Decide whether retrieval startup must verify its configured embedding model and dimension against `schema_metadata`, matching ingestion's fail-fast check.
- Calibrate representative question coverage and acceptance thresholds for model or tuning changes as corpora and models change.
- Choose metrics buckets and labels that diagnose quality and latency without exposing workspace identifiers or query text.
- Decide whether multi-turn question state belongs in the future generation service or solely in its client.

## Optional text-free storage experiment

Retained normalized documents and shared passage text are the supported storage mode. A text-free mode — vectors, structured provenance and a dedicated lexical index, with candidate text reconstructed on demand — is an unimplemented experiment and must not become the default without measured quality, availability and latency gates.

The obstacle is lexical scoring. Vectors and whole-document hashes cannot reproduce BM25: efficient scoring needs token-level postings, term frequencies, document and chunk lengths, document frequencies and corpus statistics, under the same tokenizer and scoring rules, plus positions for phrase, proximity and highlighting behaviour. Token hashes may replace dictionary strings but still require those postings, collision handling and query normalization, and they are not encryption — dictionary attacks recover low-entropy terms. The current `pg_textsearch` index is defined on `chunks.chunk_text`, so dropping that column is not a supported backend.

Reconstruction is the second obstacle. A `TextProvider` feeding reranking, exact evidence slicing, full document fetch and topic-evidence validation would need the raw hash, deterministic container recipes, extractor and configuration fingerprints, the canonical text hash and byte length, and exact placement ranges and hashes. A hash proves equality of reconstructed bytes; it cannot reconstruct them. Whitespace-collapsed passage identity is unsuitable as a lossless text address, and overlapping trimmed chunks cannot be concatenated back into a canonical document.

Adoption would require comparing retained text, losslessly compressed text and on-demand extraction; preserving mailbox headers, exact conversation relationships, topic annotations and cited spans, since removing body text alone does not remove textual metadata; treating cold OCR and replay cost against unavailable historical originals as a real expense; and turning source unavailability into an explicit omission or failure rather than an unverified citation. The acceptance bar is exact lexical, scoring and byte-slice parity, unchanged private evaluation identities, passing retrieval thresholds, deterministic tooling, and agreed cold and warm p50/p95 latency and memory bounds.
