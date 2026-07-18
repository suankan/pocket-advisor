# Pocket Advisor Roadmap

Ordered future work only. Current state lives in `docs/status.md`; shipped
roadmap history in `docs/changelog.md`; locked architecture in
`docs/design.md`, with detailed feature decisions under `docs/features/`.

## 1. Resume cutover (requires explicit user confirmation)

Scope reduced 2026-07-18 after the full test-workspace rehearsal (run
record `20260718T050815083153Z`): a from-scratch `ingest all` over 60
originals — including a Russian thread — completed in 11m06s with the
final schema, envelope payloads, tolerated OCR failures, and the
completion report. The cutover motion itself is fully rehearsed; no
pre-wipe is needed because the production workspace tree does not exist
yet and ingestion is purely additive.

1. **Russian QA: passed 2026-07-18** via the 12-question retrieval
   expectation set (all four cross-lingual questions STRONG at rank 1;
   the Russian thread's English summary verified faithful). No go/no-go
   input remains — cutover awaits only the explicit go.
2. **On explicit go:**
   `./pocket-advisor.py --workspace case-documents-demo ingest all` —
   full build from corpora including summaries and the dual vector
   index. Expected runtime from measured test rates (OCR ~3.3 s/PDF,
   summaries ~8.1 s/message, embedding ~0.27 s/chunk): roughly 2–3
   hours for ~812 emails / ~196 PDFs, interruption-safe and resumable.
   Then review the completion report, triage the review queue, and
   spot-check summaries, reply relationships, and evidence packets.
   The native `accuracy` suite is available now: after cutover, scaffold
   a production expectation set (`accuracy generate`, migrating durable
   Message-ID anchors from the legacy set), author questions, and run it.
   The test workspace's 12-question set passes 100% (2026-07-18).
3. **Follow-ups:**
   - statement parsers for the unparsed institutions (NAB, CBA, MEBank,
     AMP, Qantas cards, Revolut — ~120 of 177 production statements
     currently have no parser); the transactions stage will flag them
     loudly but honestly until each lands, re-running
     `ingest transactions` per parser;
   - after the new state validates: confirmed deletion (AGENTS.md hard
     rule 6) of the ~410 MB legacy shared-layout state
     (`workspaces/.state/cache/`, `workspaces/.state/pocket_advisor.db`)
     — currently outside native `wipe state` scope, so a one-off
     confirmed removal or a small `wipe legacy` addition.

## 2. Adapter retirement

Port the remaining frozen commands into `modules/`, then delete
`scripts/`:

- **daemon** — session-warm serving of the native relational retriever
  (one retriever everywhere; `run_search` already accepts a prebuilt
  reranker for warm reuse). Until then the frozen `daemon`/`accuracy`
  commands must not run against the fresh schema — they expect
  retired columns.
- **accuracy** — the native retrieval-expectation suite
  (generate/run/compare/list, JSON result records) shipped ahead of this
  phase. Remaining here: migrate the durable Message-ID anchors from the
  legacy production set (its integer `expect_thread` ids do not survive
  re-ingest), add the isolated `test-workspace` full rebuild/timing run,
  and A/B the shipped `envelope-v1` payload against a plain-payload
  index to measure the locked enrichment decision.
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

## 3. Local answering pass

The retrieval layer returns delimited evidence packets; the answering
pass (design sketch in `docs/features/embedding-design.md`) feeds them to a
local MLX model that produces a cited answer, shows readable source
material, and never cites a generated thread summary as evidence.

## 4. Experiments and watchlist

- **Rolling-summary quality on long threads** — a changed N-message
  thread replays N generations and the 600-token ceiling compresses
  early detail; revisit only if expectation-set spot checks or a
  long-thread collection show degradation.
- **Semantic transaction search** — bank-statement rows are structured
  but not semantically searchable; embedding normalized
  counterparty/description rows would connect Stage 5 to retrieval.
