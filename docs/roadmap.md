# Pocket Advisor Roadmap

Ordered future work only. Current state lives in `docs/status.md`; shipped
roadmap history in `docs/changelog.md`; locked architecture in
`docs/design.md`, with detailed feature decisions under `docs/features/`.

## 1. Command-scoped workspace selection

Implement the locked CLI refinement in
`docs/features/workspace-scoped-state.md`:

- make root `--workspace` conditionally enforced after parsing the complete
  command/action;
- require and resolve it for `db`, ingestion, transactions, query, daemon,
  every wipe action, blob-index, verify, and `accuracy run/list`;
- make `fetch-model` and `test` workspace-free now, without loading the
  workspace registry; preserve `accuracy compare` as workspace-free when its
  native port lands;
- reject `--workspace` on workspace-free actions rather than silently
  accepting a meaningless selector;
- keep help at every parser level state-free and preserve fail-closed behavior
  for unavailable frozen commands;
- update CLI/error-message fixtures and operational commands, then run the
  full native and frozen suites without touching live workspace state.

## 2. Resume cutover (requires explicit user confirmation)

The partial derived state predates the stable-thread/summary schema and
is intentionally refused by the engine. When directed:

1. `./pocket-advisor.py --workspace case-documents-demo wipe state` —
   confirmed immediately beforehand (AGENTS.md hard rule 6);
2. `./pocket-advisor.py --workspace case-documents-demo ingest all` — full
   re-ingest from corpora, including thread
   summaries and the dual vector index;
3. after the native accuracy command is available in item 3, run the golden-set
   checks; meanwhile spot-check cache folders,
   generated summaries, reply relationships, and readable evidence
   packets.

## 3. Adapter retirement

Port the remaining frozen commands into `modules/`, then delete
`scripts/`:

- **daemon** — session-warm serving of the native relational retriever
  (one retriever everywhere; `run_search` already accepts a prebuilt
  reranker for warm reuse). Until then the frozen `daemon`/`accuracy`
  commands must not run against the fresh schema — they expect
  retired columns.
- **accuracy** — golden-set runner over the native retriever; add the isolated
  `test-workspace` full rebuild/timing run, migrate stable expectations from
  the existing golden set, and A/B the shipped `envelope-v1` payload against
  a plain-payload index to measure the locked enrichment decision.
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

## 4. Local answering pass

The retrieval layer returns delimited evidence packets; the answering
pass (design sketch in `docs/features/embedding-design.md`) feeds them to a
local MLX model that produces a cited answer, shows readable source
material, and never cites a generated thread summary as evidence.

## 5. Experiments and watchlist

- **Rolling-summary quality on long threads** — a changed N-message
  thread replays N generations and the 600-token ceiling compresses
  early detail; revisit only if golden-set spot checks or a
  long-thread collection show degradation.
- **Semantic transaction search** — bank-statement rows are structured
  but not semantically searchable; embedding normalized
  counterparty/description rows would connect Stage 5 to retrieval.
