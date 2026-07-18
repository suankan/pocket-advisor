# Pocket Advisor Roadmap

Ordered future work only. Current state lives in `docs/status.md`; shipped
roadmap history in `docs/changelog.md`; locked architecture in
`docs/design.md`, with detailed feature decisions under `docs/features/`.

## 1. Workspace-scoped state and mandatory workspace selection

Implement the locked design in
`docs/features/workspace-scoped-state.md` before resuming cutover:

- require global `--workspace <id>` for every operational CLI command;
  top-level help remains state-free;
- resolve the selected workspace explicitly and carry it in
  `PipelineContext`; remove pipeline/retrieval dependence on implicit
  workspace selection;
- give each workspace its own bound SQLite database, cache, vectors, logs,
  and runtime tree below `workspaces/.state/workspaces/<workspace_id>/`;
- retain shared model weights only and accept complete derived-state
  duplication when collections are mounted by multiple workspaces;
- make fresh schema metadata bind a database to one workspace and reject a
  mismatched selection before mutation;
- port workspace-state wiping far enough that `--workspace <id> wipe state`
  validates, confirms, and deletes only that workspace's derived state;
- fail closed for every transitional frozen command that cannot honor the
  selected workspace; never fall back to retired shared paths;
- add two-workspace fixtures covering shared and distinct collection mounts,
  Message-ID independence, transaction rebuild isolation, scoped wipe, and
  filesystem/DB non-interference.

The existing shared state is not migrated. Implementation completion requires
the full native and frozen self-test suites plus path/custody checks; no live
workspace state is wiped as part of the implementation itself.

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
- **wipe / blob-index lookup** — finish any wipe operations not covered by
  item 1 and port blob-index lookup directly.
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
