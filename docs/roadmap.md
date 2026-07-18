# Pocket Advisor Roadmap

Ordered future work only. Current state lives in `docs/status.md`; shipped
roadmap history in `docs/changelog.md`; locked architecture in
`docs/design.md`, with detailed feature decisions under `docs/features/`.

## 1. Adapter retirement

Port the remaining frozen commands into `modules/`, then delete
`scripts/`:

- **daemon** — session-warm serving of the native relational retriever
  (one retriever everywhere; `run_search` already accepts a prebuilt
  reranker for warm reuse). Until then the frozen daemon implementation
  must not run against the fresh schema — it expects retired columns.
- **accuracy** — the native retrieval-expectation suite
  (generate/run/compare/list, JSON result records) already supersedes the
  frozen implementation. Retire the frozen accuracy code while preserving
  only fixture-independent test coverage that still adds value.
- **verify** — custody/integrity checks, plus FTS index
  self-verification: `INSERT INTO thread_summaries_fts
  (thread_summaries_fts) VALUES('integrity-check')` and the same for
  `chunks_fts`, so index/content divergence is caught mechanically.
- **wipe / blob-index lookup** — port the remaining vector-index wipe actions
  and blob-index lookup directly; workspace-state wiping is already native.
- Then delete `scripts/` and prune unused venv packages
  (`extract-msg`, `python-docx`, `openpyxl`; `beautifulsoup4` stays —
  used by emailbody).
- Move the thread-summary/query config defaults from `modules/config.py`
  into committed `config.yaml` once no frozen command strict-reads it.

## 2. Transaction parser coverage and legacy-state cleanup

These operational follow-ups are independent. They do not gate adapter
retirement, generic end-to-end platform validation, or the local answering
pass.

- Add statement parsers for unsupported institutions, currently including
  NAB, CBA, MEBank, AMP, Qantas cards, and Revolut. The transactions stage
  continues to flag every unsupported statement loudly and honestly; rerun
  `ingest transactions` for an affected workspace after each parser lands.
- Provide an explicitly confirmed way to delete retired shared-layout state
  (`workspaces/.state/cache/` and
  `workspaces/.state/pocket_advisor.db`). Native `wipe state` deliberately
  does not cover these paths, so use either a narrowly scoped one-off removal
  or add a guarded `wipe legacy` action. No platform milestone depends on
  this cleanup.

## 3. Local answering pass

The retrieval layer returns delimited evidence packets; the answering
pass (design sketch in `docs/features/embedding-design.md`) feeds them to a
local MLX model that produces a cited answer, shows readable source
material, and never cites a generated thread summary as evidence.

## 4. Experiments and watchlist

- **Envelope payload A/B** — compare the shipped `envelope-v1` recipe with a
  plain-payload index through the native retrieval-expectation suite to
  measure the locked enrichment decision.
- **Rolling-summary quality on long threads** — a changed N-message
  thread replays N generations and the 600-token ceiling compresses
  early detail; revisit only if expectation-set spot checks or a
  long-thread collection show degradation.
- **Semantic transaction search** — bank-statement rows are structured
  but not semantically searchable; embedding normalized
  counterparty/description rows would connect Stage 5 to retrieval.
