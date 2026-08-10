---
model: gpt-5.6-sol
reasoning_effort: high
---

# Retrieval quality gate

## Outcome

Pocket Advisor has a repeatable, privacy-safe evaluation workflow that measures whether retrieval returns the right evidence before model, index, chunking, fusion, reranking, or selection changes are accepted.

## Why this task is needed

The current retrieval path is implemented and exposes useful diagnostics, but it has no durable evaluation set, regression threshold, or exact-search comparison. Quality decisions therefore depend on individual queries and cannot distinguish dense-search recall, lexical recall, fusion, reranking, selection, and evidence-budget effects.

The compiled dense candidate count is 50, while pgvector documents a default HNSW `ef_search` of 40. This is a concrete reason to measure approximate-search underfill and recall rather than assume the configured candidate count is being satisfied. Retrieval startup also asserts workspace scope without checking `schema_metadata` or the BM25 index that the query requires.

The authoritative read-path design remains [`docs/retrieval-design.md`](../retrieval-design.md). This brief defines implementation work and must not become a second retrieval design.

## Priority and dependencies

This is a P0 quality task with no implementation dependency. It can proceed in parallel with the MCP access milestone, and MCP evidence-interface and broader-analysis work should reuse its cases, stage measurements, and thresholds for end-to-end acceptance.

## Scope

### Evaluation case contract

Define a versioned case format with enough information to evaluate retrieval without embedding private content in committed code or reports.

Each case supports:

| Field | Purpose |
| --- | --- |
| stable case id | compare repeated runs without using a question as an identifier |
| question | input to the normal retrieval path |
| category | exact identifier, paraphrase, bilingual, multi-topic, thread, attachment, or off-domain |
| expected sources | one or more acceptable documents identified by stable fixture id or source hash |
| topic groups | acceptable-source groups when every part of a multi-topic question must be covered |
| forbidden sources | documents whose appearance would demonstrate a known false positive |
| expected empty | assert that an off-domain query returns no packets |
| optional relevance grades | calculate ranking metrics when several acceptable documents have different value |

Commit a small synthetic corpus and evaluation set under test fixtures. Store real evaluation cases and generated reports only under a gitignored path inside `workspaces/`. Never commit real questions, source hashes, titles, paths, snippets, reports, or workspace identifiers.

Include a broad multi-document discussion case with several participants, topics, and time periods. Measure relevant-source, topic-group, participant, and conversation coverage under the default `top_k` and aggregate evidence budget, together with warning frequency and evidence omitted by that budget. This case evaluates retrieval coverage only; it does not grade an agent's prose or treat semantic top-k as proof of exhaustive discussion coverage.

### Supported evaluation command

Add a supported host command that runs evaluation for one fixed workspace, emits a human summary by default, and can emit machine-readable JSON. Keep the evaluator in a transport-independent package; the CLI is an adapter.

The command must:

- validate the case-file schema before querying;
- run cases deterministically with recorded configuration and model identifiers;
- support filtering by case id and category;
- return a non-zero status when mandatory thresholds fail;
- write private reports only to an explicitly selected gitignored location; and
- avoid logging questions, expected sources, packet text, or workspace names.

### Stage-level measurement

Expose evaluation-only observations for dense, lexical, fused, reranked, selected, and expanded results without changing the production `retrieval.Result` contract merely for testing.

Report at least:

- source recall at configurable `k`;
- reciprocal rank of the first acceptable source;
- nDCG when relevance grades are present;
- multi-topic coverage;
- broad-discussion source, topic, participant, and conversation coverage;
- forbidden-source and unexpected-result counts;
- expected-empty pass rate;
- candidate yield by dense and lexical leg;
- warning frequency;
- context-budget truncation frequency;
- evidence bytes and sources omitted by the aggregate budget; and
- per-stage and end-to-end latency distributions.

### Exact-search and HNSW comparison

Add an exact dense-search evaluation path over the same embedding namespace. Compare HNSW results with exact results for every dense evaluation case and report approximate recall.

Evaluate `hnsw.ef_search`, iterative-scan behavior, candidate count, latency, and corpus size. Configure production search only after the evaluation demonstrates an appropriate recall/latency trade-off. The production dense leg must be able to return its configured candidate count when enough matching rows exist, or emit a diagnostic that explains the limiting condition.

Use transaction- or connection-local PostgreSQL settings so evaluation and production tuning cannot leak unpredictably between pooled requests.

### Retrieval readiness

Before `--query` or an `mcp` subcommand accepts work, verify:

- the configured embedding endpoint is reachable;
- the endpoint model and dimension match `schema_metadata`;
- stored chunks do not contain an incompatible active namespace;
- the HNSW index required by dense search exists; and
- the BM25 index required by lexical search exists and is usable.

Fail readiness with an actionable error. Do not silently serve a different model namespace, dense-only retrieval, or lexical-only retrieval.

### Baselines and thresholds

Check in deterministic synthetic baselines and mandatory synthetic thresholds. Private-corpus thresholds may be configured in the private case file but must not be copied into version control if they reveal corpus characteristics.

Record enough configuration in a report to reproduce a comparison: commit id, case-set version, model identifiers, vector dimension, candidate counts, RRF value, rerank and selection settings, HNSW settings, and elapsed time. Do not include secrets, endpoints, workspace names, questions, or evidence text in the report metadata.

## Non-goals

- Do not build answer generation or grade prose answers in this task.
- Do not select a new embedding, reranking, or query-preparation model without evaluation evidence.
- Do not implement re-embedding or schema migration.
- Do not introduce a hosted evaluation service or upload private cases to a third party.
- Do not weaken workspace isolation to run cross-workspace evaluation.

## Acceptance criteria

- A committed synthetic corpus exercises exact identifiers, paraphrases, bilingual search, multi-topic decomposition, a broad multi-document discussion, thread context, attachments, and off-domain empty results.
- One supported command runs the synthetic suite and a private suite through the normal retrieval configuration.
- CI runs the synthetic suite without network access to private services or access to `workspaces/`.
- Reports distinguish every retrieval stage and include the required ranking, coverage, warning, budget, and latency measures.
- Exact dense search and HNSW results are compared and approximate recall is visible.
- Production HNSW settings have a documented evaluation result rather than relying on defaults.
- Query and MCP startup reject embedding-model, vector-dimension, or missing-index incompatibility before the first search.
- No private evaluation input or output appears in Git status after the private workflow runs.
- Existing query and MCP behavior remains covered by unit and integration tests.

## Verification

Run the repository checks from [`README.md` §9](../../README.md#9-verification), the synthetic evaluation command, retrieval unit tests, PostgreSQL integration tests, and a manual private evaluation run. Use a temporary synthetic workspace for destructive or schema-manipulation tests.

Inspect query plans for exact and HNSW evaluation queries and confirm that connection-local settings do not leak to later pooled requests.

## Documentation and handoff

Update [`docs/retrieval-design.md`](../retrieval-design.md) with the implemented readiness contract, evaluation workflow, selected HNSW policy, and metrics that remain operationally relevant. Update [`README.md`](../../README.md) with the supported evaluation command and interpretation of its exit status. Remove settled items from the retrieval open-decisions list rather than adding a decision diary.

Commit this task as one self-contained quality-gate change unless implementation reveals a justified boundary between the evaluator and production query-readiness work.

## Primary references

- [pgvector indexing, query options, filtering, and iterative scans](https://github.com/pgvector/pgvector#query-options)
