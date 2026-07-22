# Pocket Advisor Roadmap

Ordered future work only. Active work lives in `docs/work-in-progress.md`;
shipped history in `docs/changelog.md`; locked architecture in
`docs/design.md`, with detailed feature decisions in the per-concern
folders under `docs/` (`ingestion/`, `retrieval/`, `generation/`,
`inference/`, `storage/`, `benchmarks/`, `platform/`).

## 1. Transaction parser coverage

This operational follow-up is independent. It does not gate generic end-to-end
platform validation or the local answering pass.

- Add statement parsers for unsupported institutions, currently including
  NAB, CBA, MEBank, AMP, Qantas cards, and Revolut. The transactions stage
  continues to flag every unsupported statement loudly and honestly; rerun
  `ingest transactions` for an affected workspace after each parser lands.

## 2. Local answering pass

The retrieval layer returns delimited result packets; the answering
pass (locked constraints in `docs/generation/local-answering-pass.md`)
feeds them through the shared inference client to a local model that
produces a cited answer, shows readable source material, and never cites a
generated thread summary as content.

## 3. Proposed designs awaiting implementation

Docs exist for these; none is implemented. Each stays a draft until picked
up through the normal design → work-in-progress → changelog lifecycle.

- **DB/filesystem storage split** —
  `docs/storage/separate-db-and-fs-concerns.md`: move the remaining bulk
  text (`thread_summaries.summary_text`, `chunks.text`) out of SQLite to
  the content-addressed filesystem cache, leaving the database as pure
  index/statistics/linking. Requires the FTS redesign flagged inside that
  doc; migration is wipe + re-ingest only.
- **RAG gateway** — `docs/generation/rag-gateway.md`: expose the engine as
  an OpenAI-compatible `/v1/chat/completions` service so standard chat UIs
  can query the corpus. Draft proposal; depends on (or subsumes) the local
  answering pass.
- **Generation-quality evaluation** —
  `docs/benchmarks/rag-metrics-and-evaluation.md`: faithfulness/relevancy/
  correctness measurement once there is a generation pipeline to measure;
  the retrieval side is already covered by the shipped accuracy suite.

## 4. Experiments and watchlist

- **Envelope payload A/B** — compare the shipped `envelope-v1` recipe with a
  plain-payload index through the native retrieval-expectation suite to
  measure the locked enrichment decision.
- **Semantic transaction search** — bank-statement rows are structured
  but not semantically searchable; embedding normalized
  counterparty/description rows would connect the transactions stage to
  retrieval.
- **Accuracy question hardness** — generated questions can still drift toward
  envelope-adjacent phrasing (e.g. sender name); prompt filters or a small
  human-curated harder suite remain open experiments beside the ship harness.
