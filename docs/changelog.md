# Pocket Advisor Changelog

Reverse-chronological history of shipped platform changes, including completed
roadmap items. Active work lives in `docs/work-in-progress.md`; future work
lives only in `docs/roadmap.md`.

## 2026-07-26 — Concurrent streaming ingestion

Design `4240232`; implementation `d75862f`; documentation lock `73e89c6`.
Feature doc:
`docs/ingestion/concurrent-streaming-pipeline.md`.

Full `ingest all` is now a single-coordinator streaming dataflow instead of a
serial stage barrier. Discovery, email parsing, PDF-to-text production, and
embedding overlap as soon as their real inputs exist.

- Discovery hashes on a producer thread and sends typed file/collection
  events to the coordinator. Safe new candidates are parsed immediately while
  complete blob-index snapshots and changed-path integrity decisions remain
  collection-close operations.
- Parentless email bodies are compacted, chunked, and dispatched to the
  run-wide plain-text embedding queue during parsing. Replies wait only for the
  email-input close barrier needed for import-order-independent parent
  resolution. Recursive email attachments expose PDFs to OCR immediately.
- A run-scoped, CPU-bounded PDF producer accepts native and attached documents
  incrementally. Each completed transform is verified, published, chunked, and
  dispatched for embedding on the coordinator before slower PDFs finish.
- Thread reconstruction and bounded summary generation begin when email input
  closes, without waiting for outstanding PDFs. Summary completions settle in
  completion order and dispatch their vectors immediately while the
  coordinator continues polling PDF outcomes.
- SQLite, review state, canonical artifacts, and chunk/FTS publication remain
  coordinator-owned. The final embed stage is a producer-close barrier,
  missing-vector convergence, and deterministic matrix rebuild. Named-stage
  commands retain ordered-prefix behavior.
- Rich 14.1.0 now presents overlapping logical stage intervals and growing
  worker/queue totals truthfully; report durations are per-stage wall
  intervals and are intentionally non-additive.

Verification: every native `modules/tests/test_*.py` suite passes; aggregate
`uv run pocket-advisor.py test` reports 21/21. Gated regressions prove early
email dispatch before discovery closes, immediate attached-PDF offering,
PDF settlement during summary inference, completion-driven PDF publication
with growing input, coordinator-only durable writes, and order-independent
reply compaction. `git diff --check` passes.

## 2026-07-26 — Persistent Rich ingest completion and one-signal exit

Design `8d03a7d`; implementation `69e34d3`. Feature docs:
`docs/platform/runtime-observability-terminal-dashboards.md` and
`docs/platform/logging.md`.

Interactive `ingest all` now stays inside one official Rich 14.1.0 surface
from startup through the shell prompt. The final ingest audit replaces active
work in a non-transient last frame instead of clearing the dashboard and
printing a second plain report.

- The retained Rich frame contains pipeline results, performance, workspace
  state, findings, review queue, report path, execution-log path, and report
  audit time. The generic plain run banner and footer are suppressed while
  that surface owns the terminal; non-TTY output retains the stable plain
  report contract.
- The old `.interactive()` logging API is removed. `.notice()` records
  discrete operator notices and routes presentation through the active Rich
  dashboard or a Rich `Console`; `.error()` uses the same terminal boundary.
  The typed final report is installed directly and recorded once through the
  file-only channel.
- Failure and interrupt handling share one idempotent cancellation path:
  active OCR process groups are terminated, queued inference work is canceled,
  and in-flight remote calls run only on daemon workers. One Ctrl+C therefore
  returns 130 without a traceback or a second signal, even when a request is
  parked behind a remote timeout.

Verification: every native `modules/tests/test_*.py` suite passes; aggregate
`uv run pocket-advisor.py test` reports 20/20. A real Rich `Live` synthetic-TTY
test proves the final frame remains after stop; a subprocess SIGINT regression
proves blocked inference work cannot hold interpreter exit; `git diff --check`
passes.

## 2026-07-26 — Completion-driven PDF embedding dispatch

Design `1337fa2`; implementation `2199aea`. Feature doc:
`docs/ingestion/pdf-to-text-pipeline-design.md`.

PDF readiness dispatch now means actual transform readiness. Previously,
workers accumulated temporary results in memory and the coordinator waited
for the entire OCR pool before publishing any PDF text. Fast documents were
therefore held behind the slowest PDF even though the run-wide embedding
dispatcher itself was already asynchronous.

- `PdfTextStage` submits byte-ordered transform futures to the bounded pool
  and consumes them in completion order. The main coordinator publishes each
  verified text product, commits document metadata, creates chunks, and calls
  readiness embedding dispatch before waiting for the next result.
- Transform workers still write only private temporary outputs. SQLite,
  review records, canonical paths, chunk rows, and dispatch submission remain
  coordinator-owned; durable final state remains deterministic even though
  scheduling order is intentionally completion-driven.
- Cached current products and source-integrity failures are settled before
  fresh OCR starts instead of being held behind it.
- A gated regression parks one slow PDF worker and proves a faster document
  has chunks and reaches `submit_pending_leaves(..., at_readiness=True)` on
  the coordinator thread before the slow transform is released.

Verification: every native `modules/tests/test_*.py` suite passes; aggregate
`uv run pocket-advisor.py test` reports 20/20; `git diff --check` passes.

## 2026-07-26 — Live ingestion pipeline terminal dashboard

Design `32aba8f`; implementation `4d7021a`. Feature doc:
`docs/platform/runtime-observability-terminal-dashboards.md`.

`ingest all` now presents one purpose-built Rich dashboard over the real
seven-stage pipeline instead of a sequence of independent redraw bars. Rich is
the official package from `rich.readthedocs.io`, pinned to the documented
`14.1.0` release.

- **One run model** (`modules/runtime_dashboard.py`) shows workspace/run
  identity, total elapsed time, honest stage completion, per-stage
  state/duration/result, current work, inference pressure, and a bounded recent
  event area. Spinners, colour, and glyphs are redundant with visible state
  words; long results/events ellipsize for narrow terminals.
- **Truth stays with its owner**: CLI orchestration publishes stage
  transitions, existing progress objects publish task/worker/queue snapshots,
  and structured execution logging remains the durable record. Interactive and
  error records route into the live event area without changing JSONL schema or
  being recorded twice.
- **No invented workload percentage**: the header labels its 0–7 measure as
  stages; task percentage/ETA appears only with a real denominator, and a
  producer-fed inference queue shows pressure rather than a misleading ETA.
- **Real Rich lifetime**: activation requires both stdout and stderr TTYs;
  default-stream widgets correctly join the dashboard after `Live` installs
  its file proxies; explicit streams and non-TTY commands keep the legacy/plain
  renderer. The transient live region stops before the stable final report.
- **Failure isolation**: setup/render shutdown is presentation-only and
  idempotent; pipeline failure/not-run states remain distinct, and no cursor or
  live region survives an unwind.

Verification: all 20 native suites and aggregate `pocket-advisor test` pass.
A real PTY `uv run pocket-advisor.py --workspace test ingest all` completed in
1m07s (run `0be09cd6-0fea-4572-972f-cc8770a0d042`), exercised real summary
queue pressure and an oMLX `RemoteProtocolError`, continued through embed and
transactions, cleared the dashboard, and rendered the expected
`INGEST COMPLETE WITH FINDINGS` report.

## 2026-07-26 — Inference dispatch queues and live observability

Design `64ccb4b`; implemented across `491e012`, `2f80477`, `ff702b7`,
`e3b7e70`, `e202a52`, `0f3a1d5`. Feature doc:
`docs/ingestion/embedding-queue-and-workers.md`. Roadmap item 1.

Embedding work is dispatched from three producer stages and drains across
the whole run, but its state was invisible until the embed stage — outcomes
were only observed when `drain()` walked futures, long after the work. This
makes queue pressure continuously visible. **Observability only; no
throughput change.**

The draft that started this proposed decoupling embedding behind a queue so
it would start before all documents were ready. That already shipped as
readiness dispatch (`inference-serving.md` decision 5), and leaf/summary
embedding was already one pool with two counter buckets, not two queues.
Recorded as rejected so they are not re-proposed: discovery as an embedding
producer (no text exists at discovery), a queue as a speedup (email
embedding already overlaps 79% of the pipeline), and producer backpressure
(would block producers and hide the very pressure being displayed — the
docstring claiming it was false and was deleted).

- **One terminal owner** (`modules/progress.py`): `LiveDisplay` composites
  registered panels. Both progress widgets previously assumed exclusive
  ownership of the bottom of stderr, which held only because bars were
  strictly sequential; a run-long queue row is concurrent with them by
  definition. A finished bar scrolls its summary permanently above the live
  region and pinned rows keep drawing below. Per-widget heartbeat threads
  collapse into one. Locking rule: the display lock is innermost — `lines()`
  is cached and lock-free, `refresh()` is called only from outside it.
- **`BoundedInferenceDispatcher`** (`modules/dispatch.py`) under both
  fan-outs, which had duplicated pool, futures, latch, and lifecycle;
  `summary_dispatch.py` had been importing a private `_LIVE` across a module
  boundary and carrying an admitted "unused placeholder for symmetry" lock.
  The pools stay separate: the embedding and summarisation endpoints are
  independent capacity budgets.
- **Worker-side counters** with an immutable `QueueSnapshot`, so state is
  observable during a run rather than at drain.
- **One embedding dispatcher per run**: the embed stage's convergence pass
  reuses the readiness dispatcher via a barrier `drain()` instead of
  building a second one, so counters span both phases. `retarget()` refuses
  a non-idle dispatcher — the convergence sweep decides pending work by
  globbing the vector directory, so an entity still being written would
  otherwise be dispatched twice.
- **`pending_entities` → `processed_entities`**: the field is incremented on
  completion, so the old name inverted its meaning and made the validation
  invariant unreadable. Saved-report schema change.

Verification: 19/19 suites pass, three new. The barrier was checked by
mutation — removing it fails the suite on the idle guard, turning a silent
double-embed into a loud error. Concurrent rendering was driven through a
pseudo-terminal and replayed in-process through an ANSI interpreter. Not
verified end-to-end against a running oMLX instance over a live workspace.

## 2026-07-26 — Structured execution logging

`docs/platform/logging.md` implemented in full (proposed, reviewed twice,
locked, and built the same day). `modules/logs.py` is now the single entry
point for all operator-facing output — structured records, terminal lines,
and progress bars alike — over a stdlib `logging` engine.

- **Four methods, destination by method** (`modules/logs.py`):
  `.interactive()` (terminal + file), `.error()` (stderr + file, with
  `exc_info`), `.info()` and `.debug()` (file only). `.info()` being
  file-only is what lets instrumentation grow without degrading the
  terminal; `LOG_LEVEL` gates volume, not destination.
- **One JSON-lines file per invocation** at
  `workspaces/.state/workspace-<id>/execution-logs/<YYYYMMDD-HHMMSS>.jsonl`.
  Six fields: `timestamp` (RFC 3339, UTC, millis), `run_id`,
  `worker_thread` (thread name), `caller`, `level`, `message`, plus
  free-form keyword fields. Reserved names — including `exc_info`, whose
  misuse silently dropped tracebacks — are rejected at call time.
- **Reachability without a ctx**: `get_log()` is configured once by
  `setup_logging()` at the CLI entrypoint, so `modules/inference.py` — the
  layer whose failure motivated this work — can record the endpoint and
  transport exception. `PipelineContext.log` aliases the same object.
- **Progress bars are facade-owned** (`log.progress()`,
  `log.worker_pool()`): nine call sites across seven modules no longer
  import `modules/progress.py`. Bars emit one lifecycle record on `done()`,
  never per-redraw, and terminal output routes through the live bar so no
  redraw is shredded.
- **`httpx`/`httpcore` captured at DEBUG** under `LOG_LEVEL=debug` only,
  correlated by the same `run_id`.
- **`run_id` correlation**: announced once as a terminal banner and repeated
  in a `finally` footer, and carried on `IngestRunReport`
  (`REPORT_SCHEMA_VERSION` 4 → 5) so a saved report pivots to its log.
- **Preserved across `wipe state`** via `PRESERVED_STATE_NAMES`: the record
  of what went wrong must outlive the recovery step.

Verification: native suite 16/16 and `pocket-advisor test` 16/16; two real
`ingest all` runs against a dead endpoint. Under `LOG_LEVEL=debug`, 85
`httpcore` records placed the motivating `RemoteProtocolError` at
connection level, per-thread and per-millisecond across four `summary-gen`
workers. Replaying a run's `interactive` records reproduced its terminal
transcript exactly; report and log `run_id` matched; terminal output was
structurally identical between `info` and `debug`. One measured behaviour
change: two pipeline error lines moved from stdout to stderr, where
`.error()` puts them — nothing was lost from the two combined. `wipe state`
preservation and the `KeyboardInterrupt` path are covered by unit test
rather than end to end (the former is destructive and was not authorised;
a real SIGINT was absorbed by the app's existing interrupt handling).

Roadmap item 1 removed; remaining items renumbered and cross-references
repaired.

## 2026-07-26 — DB/filesystem storage split shipped

`docs/storage/separate-db-and-fs-concerns.md` implemented in full (locked
2026-07-24, picked up the same day). SQLite is now strictly an index /
statistics / linking engine; every piece of bulk derived text lives on the
filesystem or isn't stored at all.

- **Slicing reader** — `modules/chunk_reader.py` (`ChunkReader`) slices
  chunk text on demand from the parent artifact (email body region or
  extracted document text) given a chunk row + config; offsets are
  code-point based. `modules/summary_reader.py` (`read_summary_text`)
  reads the parallel summary file.
- **Schema D** (`modules/database.py`) — dropped `chunks.text` and the
  payload/translit shadow columns; dropped `thread_summaries.summary_text`
  and `generator_model`, added `summary_sha256`; both FTS tables
  (`chunks_fts`, `thread_summaries_fts`) are now contentless
  (`content='', contentless_delete=1`) with no triggers; legacy-schema
  detection refuses a database with a stored `chunks.text` or
  `thread_summaries.summary_text` column and points at `wipe state`.
  Added `fts_feed_state` to back a drop-and-refeed convergence pass in
  place of migrations.
- **Producers** — `modules/embedding/chunks.py` feeds `chunks_fts`
  explicitly at insert time from a computed slice + shadows;
  `sync_payloads` is a convergence pass keyed on `fts_feed_state` and
  rowid/index count parity, not a per-chunk recompute.
  `modules/pipeline/summaries.py` writes
  `summaries/<thread_id>/summary.txt` via `write_verified`, stores
  `summary_sha256`, and feeds `thread_summaries_fts` explicitly (including
  the staleness-sweep deletion path). `thread_vector_filename`
  (`modules/embedding/backends.py`) now keys on `summary_sha256` instead
  of summary text.
- **Consumers** — `modules/retrieval.py`, `modules/pipeline/embed.py`,
  `modules/ingest_report.py`, `modules/maintenance.py` all read through
  `ChunkReader`/`read_summary_text`; the `payload_shadow` coverage metric
  is replaced by FTS rowid-count parity; `maintenance.py` gained
  `_verify_summaries` (hash-verifies every current summary file) and
  dropped the unsupported `'integrity-check'` command for both contentless
  indexes.
- **Migration is wipe + re-ingest only**, as designed — no in-place
  backfill path exists or was needed.

Verification: full native suite and `pocket-advisor test` green,
`git diff --check` clean, and an operator-run `wipe state` + `ingest all`
+ `verify` + `accuracy run`/`compare` pass confirmed rank-identical search
results and unchanged accuracy scores against the pre-change record on the
test workspace (acceptance criteria 1–8, `separate-db-and-fs-concerns.md`).
Roadmap item 3 bullet removed; design doc locked to **implemented**.

## 2026-07-23 — New design docs: Corpus API proposal, thread-summaries consolidation

No code changes. Two documents added out of the architecture review:

- `docs/retrieval/corpus-api.md` (**proposed**, operator-agreed direction):
  a canonical `email.json` manifest per email (headers, body references,
  attachment identities — a manifest beside the text artifacts, not a body
  container; the relational index stays per the storage scope rule), and
  one typed `Corpus` facade with two interface families — deterministic
  SQL-backed getters and semantic model-engaging getters. Stepping stone
  to the RAG gateway. Roadmap item 3.
- `docs/ingestion/email-thread-summaries.md` (**implemented design,
  consolidated**): one-place explanation of what email-thread summaries
  are (email-only by construction — eligibility is a count of emails per
  thread; renamed from `thread-summaries.md` the same day to say so), why
  they exist (thread-grain recall channel), their digest-gated lifecycle,
  the two fences that keep them navigation-only, and the operator-review
  concerns recorded 2026-07-23. Carries three roadmapped TODOs: runtime
  summarization methods on the retrieval engine (TODO 1), a
  summary-ablation accuracy methodology with a thread-grain question class
  and an explicit keep-or-retire outcome rule (TODO 2), and a code-wide
  `thread` → `email_thread` terminology rename to stop the collision with
  Python threading (TODO 3 — notes the fresh-schema cutover and
  preserved-test-data implications).
- `docs/design.md` feature index and `docs/roadmap.md` items 3–4 updated
  accordingly.

## 2026-07-23 — Full design/code alignment audit

No functional code changes (two stale docstring comments only). Audited
every design doc against the implemented code, classifying each
misalignment as *superseded by implementation* (removed) or *never
implemented but worth keeping* (kept, added to roadmap):

Superseded content removed/corrected:

- `docs/design.md` + `docs/storage/workspace-scoped-state.md`: the retired
  `fetch-model` action and "model download" were still described as
  workspace-free actions; the engine downloads no models since the oMLX
  cutover.
- `docs/retrieval/query-daemon.md`: rationale still described in-process
  MLX model loading ("MLX backends are not assumed thread-safe", "models
  load once"); now matrices + a warm inference client, with model warmth
  the server's concern.
- `docs/benchmarks/accuracy-testing.md` (+ `modules/accuracy.py`
  docstring): "local MLX model" and "model id" in result/environment
  records → the configured summarisation endpoint and prompt version,
  matching what `persist_result` actually writes.
- `docs/ingestion/transaction-domain-design.md`: schema and
  `reconciliation.yaml` examples still used the retired `items` table
  (`item_id`) — now `documents`/`document_id`, matching Schema C and
  `load_reconciliation`.
- `docs/ingestion/ingest-all-reporting.md`: contract said "schema version
  2 locked" while code is at version 4 (`REPORT_SCHEMA_VERSION`); embed
  telemetry example fields (buckets/microbatches/padding/bisections) were
  retired at the oMLX cutover in favor of the service-execution counters in
  `modules/telemetry.py`; "future `verify` command" → `verify` is
  implemented.
- `docs/ingestion/ingestion-performance.md`: rewritten. Removed the
  superseded Workstream B local micro-batching machinery
  (`bucket32-batch8-v1` — retired with in-process embedding), the
  superseded 2-workers×5-jobs PDF topology (replaced by
  `pdf-to-text-pipeline-design.md`'s full-core `--jobs 1` pool), the stale
  schema-v2 JSON block, and the session-warm-model detail. Kept what still
  governs: the 48k one-shot / 24k segment / 16-way reduce thresholds
  (`modules/summarization.py`), content-addressed transform identity, and
  the historical measured baseline.
- `docs/generation/rag-gateway.md` and
  `docs/benchmarks/rag-metrics-and-evaluation.md`: stripped LLM-chat
  preamble cruft and added explicit not-implemented status headers.

Never-implemented, kept and roadmapped (`docs/roadmap.md` new item 3
"Proposed designs awaiting implementation"): the DB/filesystem storage
split (`docs/storage/separate-db-and-fs-concerns.md`), the RAG gateway
draft, and generation-quality evaluation.

Verified consistent with code and left unchanged: ingestion-design-v2,
pdf-to-text-pipeline-design, transaction-stage-convergence (`pdfinfo`
fingerprint confirmed in `transactions.py`), summary-generation-concurrency,
chunking-and-embedding, hybrid-retrieval-and-ranking, inference-serving,
query CLI/daemon howtos, and the summary thresholds
(`SUMMARY_ONE_SHOT_TOKENS`/`SUMMARY_SEGMENT_TOKENS` confirmed at 48k/24k).

## 2026-07-23 — Design lock: OOP-style Python

`docs/design.md` "Runtime and code boundaries" now locks the existing
class-based convention as an explicit preference: classes are the default
unit of design (stage classes, frozen domain dataclasses, stateful concerns
as lifecycle-owning classes); module-level functions are reserved for small
pure helpers. Documents what the codebase already practices — no code
changes.

## 2026-07-23 — Docs restructured into RAG-pipeline folders

No functional code changes (docstring path references only). Restructured the
flat `docs/features/` list into seven concern folders mirroring the canonical
RAG pipeline split, per a working plan document (`design-split-plan.md`,
since deleted as it instructed once the restructuring stabilized).
The folders live directly under `docs/` (the intermediate `features/` level
is removed entirely):

- **`ingestion/`** — `ingestion-design-v2.md`, `pdf-to-text-pipeline-design.md`,
  `ingest-all-reporting.md`, `ingestion-performance.md`,
  `transaction-stage-convergence.md`, `summary-generation-concurrency.md`,
  `transaction-domain-design.md` (non-RAG rider on the ingest pipeline), and
  the new `chunking-and-embedding.md`.
- **`retrieval/`** — `query-daemon.md` and the new
  `hybrid-retrieval-and-ranking.md`.
- **`generation/`** — `rag-gateway.md` (draft candidate) and a new
  `local-answering-pass.md` stub holding the locked constraints for the
  not-yet-implemented answering pass.
- **`storage/`** — `workspace-scoped-state.md`,
  `separate-db-and-fs-concerns.md`.
- **`benchmarks/`** — `accuracy-testing.md`, `rag-metrics-and-evaluation.md`.
- **`platform/`** — `uv-migration.md`.
- **`inference/`** — the new `inference-serving.md` (shared by all three
  pipelines; its own bucket rather than a pipeline folder).

`embedding-design.md` (yesterday's three-doc merge) was split along pipeline
seams into `ingestion/chunking-and-embedding.md` (decisions 1–10, schema,
summary stage, dense index layout — decision 9's number preserved for
`storage/separate-db-and-fs-concerns.md`'s citation),
`retrieval/hybrid-retrieval-and-ranking.md` (four-leg RRF fusion, rerank,
packet expansion), and `inference/inference-serving.md` (oMLX client,
endpoints, decisions 5/6 numbering preserved for code-docstring citations);
its forward-looking answering-pass material became the `generation/` stub.
The original file is deleted.

`docs/design.md` was reorganized around the same three pipeline sections
(Ingestion / Retrieval / Generation-not-yet-implemented) plus cross-cutting
concerns, with its feature index grouped by folder. Every path reference
across `docs/` and `modules/` docstrings was updated to the new locations,
including historical changelog entries for files that merely moved;
references to deleted filenames (`embedding-design*.md`,
`inference-endpoints.md`) remain by their historical names.

## 2026-07-23 — Doc merge: embedding-design.md absorbs v2 and inference-endpoints

No functional code changes. At the operator's request, folded
`docs/features/embedding-design-v2.md` (oMLX execution model) and
`docs/features/inference-endpoints.md` (endpoint config surface) into
`docs/features/embedding-design.md`, which is now the single authoritative
document for retrieval semantics, the oMLX inference-server design, and the
current endpoint-based config. Both source files are deleted.

- The three-way split had already drifted out of sync with shipped code more
  than once (loopback enforcement, model-name config keys, and the vector
  fingerprint's `model` field all disagreed across the two inference docs
  before yesterday's fixes). Merging removes that structural risk.
- The merge surfaced two more stale passages that the split had hidden inside
  `embedding-design.md` itself: a "Dense index layout" paragraph still
  describing the retired local `bucket32-batch8-v1` tokenize/bucket/batch/
  bisect recipe, and a "passage vs. query embedding" asymmetry claim that
  oMLX's instruction-dropping behavior had already made false. Both are
  corrected in the merged doc.
- Preserved decision numbers referenced elsewhere: decision 9 in the
  retrieval-semantics list (cited by
  `docs/storage/separate-db-and-fs-concerns.md`) and decisions 5/6 in the
  inference-server list (cited by docstrings in `modules/inference.py`,
  `modules/pipeline/embed.py`, `modules/pipeline/emails.py`,
  `modules/pipeline/pdfs.py`, `modules/pipeline/base.py`,
  `modules/embedding/dispatch.py`) are unchanged.
- Updated every `embedding-design-v2.md`/`inference-endpoints.md` reference
  across `docs/` and `modules/` (8 code docstrings, `docs/design.md`'s
  feature index, `docs/rag-dev-howto.md`, and
  `docs/ingestion/summary-generation-concurrency.md`) to point at the merged
  `embedding-design.md`.
- Condensed the two docs' "Migration from current code"/"File changes"
  tables into a single "Implementation history" section; full dated detail
  remains in this changelog under "Inference-server (oMLX) cutover"
  (2026-07-20), "Endpoint-based inference configuration" (2026-07-22), and
  "oMLX alias routing" (2026-07-22).

## 2026-07-22 — Doc cleanup: align design docs with implemented code

No functional code changes. Read every module under `modules/` as source of
truth, then reconciled `docs/` against it:

- Left `docs/generation/rag-gateway.md` and
  `docs/benchmarks/rag-metrics-and-evaluation.md` in place at the operator's
  request: draft material kept as candidates for future revision/
  implementation, not a description of current code. Neither has any
  corresponding implementation today (no gateway process, no ragas-style
  metrics), and both are deliberately absent from `docs/design.md`'s feature
  index for that reason. The real, implemented retrieval-accuracy system
  remains documented in `docs/benchmarks/accuracy-testing.md`.
- Updated `docs/ingestion/summary-generation-concurrency.md` status from
  "proposed" to shipped — `modules/pipeline/summary_dispatch.py` already
  implements the design exactly (commit `b884ed1`), and the changelog
  already carried its ship entry.
- Updated `docs/platform/uv-migration.md`: the old `venv/` directory is
  confirmed deleted, so the doc no longer carries that operator TODO.
- Reconciled `docs/features/embedding-design-v2.md` decisions 1, 2, 6, and
  8 and its Config surface section with the later endpoint-based config
  cutover (`docs/features/inference-endpoints.md`): no loopback
  enforcement, no server-id model config keys, no `model` field in the
  vector fingerprint, and readiness checks are reachability-only. Added a
  cross-reference note at the top of the document.
- Fixed `docs/features/inference-endpoints.md` decision D2, which predated
  and was contradicted by commit `3142d1f` (fixed concern-alias `model`
  field required by oMLX) — see the entry above.
- Fixed `docs/features/embedding-design.md`'s example config, which still
  showed the retired `mlx_model_thread_summary` key.
- Added `docs/ingestion/summary-generation-concurrency.md` to
  `docs/design.md`'s feature index (it was shipped but never indexed).
- Fixed `docs/rag-dev-howto.md`: a doubled `uv run uv run` typo (two
  places), a stale `models.inference_endpoint`/`embedding-design-v2.md`
  reference (now points at `inference-endpoints.md`), and a directory-layout
  diagram/description still describing the pre-ingestion-design-v2
  `cache/`, `pdf-original/`, `pdf-ocr/` layout instead of the actual
  content-addressed `emails/<sha256>/` / `documents/<sha256>/{source,transforms}`
  layout.
- Made `docs/rag-user-howto.md`'s command examples consistently use
  `uv run pocket-advisor.py`, matching every other doc post-uv-migration.

## 2026-07-22 — oMLX alias routing: model field in inference requests

Implementation commit: `3142d1f` (design `docs/features/inference-endpoints.md`
decision D2, updated).

- oMLX's request schema requires a `model` field even without server-id
  configuration. `modules/inference.py` now sends a fixed, non-configurable
  literal per concern — `"embedding"`, `"reranker"`, `"summariser"` — matching
  the alias each endpoint is configured under in oMLX's own
  `model_settings.json`. This is not a user-configurable model name; the
  engine still knows nothing about real model ids or weights.

## 2026-07-22 — Endpoint-based inference configuration

Implementation commit: `527fd25` (design
`docs/features/inference-endpoints.md`).

- Replaced `models.inference_endpoint` + `model_embed_text` +
  `model_rerank` + `model_thread_summary` with three user-configurable
  endpoint URLs: `embedding_endpoint`, `reranker_endpoint`,
  `summarisation_endpoint`. The engine sends no model names in requests;
  users swap models on the server side (oMLX admin panel or any
  compatible API). Remote and paid endpoints are allowed — no loopback
  enforcement. `embed_dim` is auto-detected from the first embedding
  response.
- Removed `generator_model` from thread-summary staleness check
  (`prompt_version` is the sole staleness authority). Old config keys
  are silently deprecated.

Verification: full native suite 14/14 and `uv run pocket-advisor.py
test` 14/14 pass; `git diff --check` clean. No collection content
modified.

## 2026-07-22 — venv-to-uv runtime migration

Implementation commit: `3a7f8b0` (design `dc97b19`,
`docs/platform/uv-migration.md`).

- Replaced `venv` + `requirements.txt` + `pip` with `uv`. Introduced
  `pyproject.toml` (`requires-python >=3.14`, 7 runtime dependencies) and
  committed `uv.lock` for reproducible installs. Setup is now `uv sync`;
  invocation is `uv run pocket-advisor.py`.
- Removed the `os.execv` venv auto-re-exec logic from `pocket-advisor.py`
  (deleted `os`/`Path` imports, `VENV`/`VENV_PYTHON` constants, and the
  re-exec block). `uv run` handles environment transparently.
- Deleted `requirements.txt`. Updated `AGENTS.md`, `docs/rag-dev-howto.md`,
  `docs/features/embedding-design-v2.md`,
  `docs/ingestion/summary-generation-concurrency.md`, and `.gitignore`
  (`venv/` → `.venv/`).

Verification: full native suite 14/14 and `uv run pocket-advisor.py test`
14/14 pass; `git diff --check` clean. No collection content modified.

Operational note: the old `venv/` directory should be deleted manually by the
operator after confirming the uv environment works.

## 2026-07-22 — Doc cleanup: merge old specs, consolidate howtos, delete dead artifacts

No functional code changes. Documentation-only cleanup:

- Merged old `docs_old/CHANGELOG.md` into `docs/changelog.md` as condensed
  "Pre-rewrite history" section
- Merged `docs_old/specs/structured-transactions-v2.md` into
  `docs/ingestion/transaction-domain-design.md`; purged legal/custody/evidence
  terminology
- Moved `QUERY.md` into `docs/rag-user-howto.md` (RAG query contract,
  citations, daemon management)
- Moved `RUNBOOK.md` into `docs/rag-dev-howto.md` (setup, ingestion,
  maintenance, accuracy testing, verification)
- Deleted `docs_old/` entirely (dead pre-rewrite design docs and specs)
- Deleted `models/` folder (6.8GB dead Jina/Qwen weights; all inference is
  HTTP to oMLX)
- Cleaned `.gitignore` (removed stale `models/` entry, fixed `evidence`
  comment)
- Updated stale local-model references in design docs to reflect oMLX
- Zero broken references; 14/14 tests pass

## 2026-07-20 — Thread-summary generation concurrency

Design lock: `17be322`; implementation commit: `b884ed1` (design
`docs/ingestion/summary-generation-concurrency.md`; user-directed platform
change, not a numbered roadmap item).

- The summaries stage no longer generates thread summaries one at a time. A
  new `EmailThreadsSummaryDispatcher` (`modules/pipeline/summary_dispatch.py`)
  fans the stale-thread generation loop out across a bounded pool
  (`max_in_flight` defaults to `INFERENCE_MAX_IN_FLIGHT = 8`), so up to eight
  long-context 4B decodes run concurrently against oMLX's continuous batching
  instead of strictly back-to-back.
- Generation logic (`_generate`/`_call_generator`/`_reduce`/`_structural_segments`
  and the render/split helpers) moved to `modules/pipeline/summaries_core.py`
  as worker-safe, progress-free, telemetry-free functions; the stage's `run`
  loop now submits all stale threads, `drain()`s, then settles each outcome on
  the main thread (DB upsert + commit + `submit_summary` for its embedding +
  telemetry merge + failure flag). Only `generator.generate()` runs off-thread;
  the sqlite connection, `Progress` bar, and `ReviewLog` are never touched from
  a worker.
- The dispatcher shares the embed dispatcher's weakref registry
  (`modules/embedding/dispatch.py`'s `LiveDispatcher` protocol), so the existing
  `cancel_all()` interrupt hook abandons in-flight generation with no `cli.py`
  change. Unreachable oMLX (`InferenceUnavailable`) degrades to `skipped`
  (durable pending gap), not `failed`, matching embedding dispatch.
- `test_summary_performance.py` adds concurrency, unavailable-skip, and
  per-thread-failure-isolation tests; full suite (14/14) and `./pocket-advisor.py
  test` pass.

## 2026-07-20 — Inference-server (oMLX) cutover

Design lock: `e870d46`; implementation commit: `b2d06a4` (design
`docs/features/embedding-design-v2.md`; user-directed platform change, not a
numbered roadmap item).

- All inference — embedding, summarization, reranking, and accuracy question
  generation — now calls the external localhost oMLX server (OpenAI-compatible
  `/v1/embeddings`, `/v1/rerank`, `/v1/chat/completions`) through one thin
  synchronous client (`modules/inference.py`). The engine loads no models,
  imports no `mlx`, and `requirements.txt` carries no MLX stack; `httpx` is
  the only new dependency. The client refuses non-loopback endpoints
  (mechanical enforcement of the local-only rule) and fail-fasts with a clear
  "is oMLX running?" error including a `GET /v1/models` model-id check.
- Models switched to symmetric Qwen3 served ids: `Qwen3-Embedding-0.6B-8bit`
  (dim 1024), `Qwen3-Reranker-0.6B-4bit`, `Qwen3.5-4B-MLX-4bit`. Config keys
  renamed to `models.inference_endpoint` / `model_embed_text` / `model_rerank`
  / `model_thread_summary` / `embed_dim`; the retired `mlx_model_*` keys and
  the `fetch-model` action are rejected, not aliased. Jina v5 is retired; its
  vector caches remain inert on disk.
- Producers dispatch embeddings at artifact readiness (`modules/embedding/
  dispatch.py`): the emails stage chunks and dispatches authored bodies after
  compaction, the PDF stage dispatches each document the moment its text
  product publishes, and the summaries stage dispatches each regenerated
  summary vector on upsert — bounded ≤8 in flight, best-effort (an
  unreachable endpoint prints one warning and leaves pending gaps). The embed
  stage is the loud convergence pass: sweep, backfill, review-flag failures,
  rebuild matrices. Chunk creation moved to `modules/embedding/chunks.py`,
  shared by producers and the convergence sweep.
- Vector identity: fingerprint `backend: omlx`, model-agnostic
  `execution_recipe: omlx-v1`; dim comes from `models.embed_dim` and is
  asserted against every embedding response. The local
  `bucket32-batch8-v1` tokenize/bucket/batch/bisect machinery and the Jina
  MLX loader (`modules/embedding/loader.py`, `ModelStore`) are deleted.
- Summarization/question prompts, versions, digests, and staleness semantics
  are unchanged; the generator-model id change itself regenerates all
  summaries and generated questions through the existing comparison. Token
  budgeting uses the deterministic `ceil(chars/3)` estimate; exact telemetry
  tokens come from service `usage` fields. Ingest-report schema bumped to v4
  (embed queue counters: pending/input_tokens/dispatched_at_readiness/
  successful/failed).
- Verification: native suite 14/14 with hermetic stubs (producer fixtures
  disable dispatch; embed/retrieval tests patch the backend seam); all three
  live oMLX endpoints verified by hand, including `chat_template_kwargs.
  enable_thinking=false` suppression and rerank ordering. Deferred: the full
  test-workspace re-embed plus `accuracy run` thread-or-better baseline under
  symmetric Qwen3 (in progress at the time of this entry) — record the new
  baseline before relying on retrieval quality comparisons.

## 2026-07-19 — PDF-to-text pipeline, interrupt-safe shutdown, core-count workers

Implementation commit: `bf62292` (roadmap item: 1. PDF-to-text pipeline; design
`docs/ingestion/pdf-to-text-pipeline-design.md`).

- Replaced the inherited nested OCR topology with a shared work-stealing queue
  over the content-addressed document graph. Documents are admitted
  largest-first so heavy PDFs are pulled early; each worker runs
  `ocrmypdf --jobs 1` then `pdftotext -layout`. The coordinator is the sole
  SQLite/publication/review owner and publishes deterministically by
  `document_id` (`ocr.py`, `pdf_transforms.py`, `pipeline/pdfs.py`).
- Added `WorkerPoolProgress` with a per-worker current/total line and per-job
  timer (`progress.py`), wired into the PDF stage.
- Made Ctrl+C interrupt-safe: a one-shot global interrupt flag, prompt child
  process termination, and pool unwind that skips remaining queued PDFs;
  `cli.main` catches `KeyboardInterrupt` and exits with code 130 plus a single
  `KeyboardInterrupt — interrupted` line (no traceback).
- Removed the `ingestion.pdf_workers` config knob. Workers are now fixed at
  `min(logical CPU cores, pending PDF count)` — a deliberate political decision
  after benchmarking showed linear wall-time scaling with worker count and no
  memory pressure on hundreds of PDFs. Each worker runs one `--jobs 1` ocrmypdf
  child, so the pool is the sole parallelism axis.
- Extended ingest-report telemetry (schema v3) with pending-admission bytes,
  text-only/unchanged/OCR-warning document counters, and queue-wait totals
  (`telemetry.py`, `ingest_report.py`).

Verification: full native module suite and `./pocket-advisor.py test` pass
14/14; `git diff --check` is clean. No collection content was modified.

## 2026-07-19 — Content-generated accuracy questions

Implementation commit: `a6557fe` (design `ff5ddfd`; roadmap item:
Content-generated accuracy questions; design
`docs/benchmarks/accuracy-testing.md`).

- Replaced scaffold-only `accuracy generate` (TODO placeholders) with local-MLX
  synthesis of complete natural-language questions from graph-owned authored
  email bodies and PDF text products. Subjects, filenames, and thread summaries
  are never supplied as generator input. The default artifact is
  `expectations/generated.yaml` with durable anchors, `origin: generated`, and
  scorable questions; overwrite requires `--force`; optional `--limit N` trims
  a deterministic candidate list.
- Added `modules/question_generation.py` (reuses the local
  thread-summary model, `QUESTION_PROMPT_VERSION = 1`, greedy decode) and
  wired generate progress reporting. `accuracy run` remains warm
  retrieval-anchor scoring only and records `question_generator` identity in
  the result environment when the suite is machine-generated.
- Fixture coverage uses a fake generator for the full generate → run →
  compare → list cycle. Live isolated test-workspace smoke: generate 25/25 in
  ~1m07s, then `accuracy run` 25/25 scored, 0 miss, 100% thread-or-better.

Verification: full native module suite and `./pocket-advisor.py test` pass
14/14; `git diff --check` is clean. No collection content was modified.
Generated expectations and result records live under the preserved test
workspace suite path only.

Deferred: optional hardness experiments (stricter anti-envelope prompt filters
or a small human-curated harder suite) remain on the roadmap watchlist; they
do not block the shipped regression harness.

## 2026-07-19 — Content-addressed content graph

Implementation commit: `88fc235` (roadmap item: Ingestion design v2;
design `docs/ingestion/ingestion-design-v2.md`).

- Replaced the retired item/membership attachment schema with a fresh,
  workspace-local graph of unique raw-email identities, unique retained binary
  documents, native source occurrences, and explicit attachment occurrences.
  Attached emails and ZIP members retain their complete multi-parent lineage
  through attachment rows rather than a lossy scalar parent field.
- Materialized one verified email artifact pair per email SHA and one verified
  document source/product namespace per document SHA. Repeated source paths,
  native PDFs, and email attachments remain independently citable relational
  occurrences without per-email attachment-copy dependency.
- Moved threading, chunks, hybrid retrieval, citation expansion, PDF products,
  embeddings, maintenance/verification, ingest reporting, accuracy, and
  transactions to direct email/document identities. Stage 5 now reports a
  discovery-visible native statement with no current document text as
  `NOT INGESTED`, rather than silently excluding it.
- Added fresh-schema refusal for retired databases and isolated coverage for
  duplicate raw emails/source paths, duplicate documents, repeated attached
  emails, ZIP lineage, native/email document convergence, graph retrieval,
  integrity verification, reporting, PDF freshness, and transaction rebuilds.

Verification: full native module suite and `./pocket-advisor.py test` pass
14/14; Python compilation and `git diff --check` are clean. No real workspace
state or collection content was modified. A selected real-workspace fresh
rebuild remains operator-owned and requires explicit confirmation immediately
before `wipe state`; preserved `search-accuracy-tests/` remains outside that
regenerable deletion.

## 2026-07-19 — Content-addressed PDF transforms and bounded concurrency

Implementation commit: `ce6c27f` (Workstream C in
`docs/ingestion/ingestion-performance.md`).

- Added a workspace-local canonical transform cache keyed independently by
  source SHA-256, OCR recipe, and text-extraction recipe. Exact duplicates now
  transform once; a text-only recipe change reuses the verified OCR product,
  while an OCR change rebuilds both layers. Strict sidecar manifests and
  product hashes reject missing, corrupt, mismatched, or unknown state.
- Preserved occurrence-level integrity and citations: canonical products fan out
  as independently hashed, atomically published plain copies to every existing
  `pdf-ocr/` and `pdf-to-text/` location. No hardlinks or pointer-only
  occurrence artifacts are used; missing local artifacts repair from verified
  canonical state without rerunning external tools.
- Added deterministic bounded scheduling. Workers receive only verified
  workspace-cache originals, write temporary outputs, and return typed
  results; SQLite, review logging, warnings, and fan-out remain coordinator
  owned. The configured worker/jobs product cannot exceed the local CPU
  budget. Timeout and interrupt handling terminates complete OCR process groups
  and leaves successful canonical products independently resumable.
- Strengthened schema-2 PDF telemetry reconciliation for unique outcomes,
  duplicate reuse, configured/observed workers, and nested CPU allocation.
  Streaming integrity copies avoid materializing large derivative PDFs in
  memory, and fallback to the verified original still requires a successful,
  present, readable `pdftotext` result.

Verification: the same four unique workspace-local PDFs were read without
modification and all benchmark output was written to temporary state. On the
10-core supported host, 1 worker × 10 OCR jobs took 27.029s, 4×1 took 44.128s,
and the selected 2×5 topology took 22.493s (1.20x faster than the sequential
baseline). Synthetic fixtures cover exact duplicates, independent inodes,
OCR-only/text-only invalidation, fallback and failure retry, canonical repair,
bounded scheduling, and strict telemetry. The complete native module loop,
full synthetic `verify` invariants, and `./pocket-advisor.py test` pass 14/14;
`git diff --check` is clean. No collection content or live workspace state
was mutated.

Deferred: cross-stage CPU/GPU overlap remains an explicit non-goal because the
new per-stage mechanisms remove redundant work without complicating stage
ordering, SQLite ownership, or failure reporting. The next normal PDF run in
each workspace performs one `pdf-text-v3` recipe convergence rather than
migrating older per-occurrence derivatives; this is normal derived-state work,
not a roadmap gate.

## 2026-07-19 — Shape-stable embedding microbatches

Implementation commit: `857d98e` (Workstream B in
`docs/ingestion/ingestion-performance.md`).

- Replaced one-model-call-per-passage execution with independent leaf and
  summary queues using repository-owned 32-token buckets and microbatches of
  eight. Model inputs include the real Jina task prefix, tokenize once, retain
  explicit entity ordering, and use padding attention masks; `embed_one()`
  remains the query and isolated-fallback path.
- Added recursive batch bisection. A failing member is retried individually,
  while successful peers publish durably and remain available to crash-resume;
  telemetry now reports occupied buckets, model batches, real/padded tokens,
  bisections, individual fallbacks, and all three embed subphase timers.
- Made `bucket32-batch8-v1` part of vector fingerprint identity because batched
  execution is semantically equivalent but not bit-identical on mixed-length
  inputs. The locked same-hardware rule is maximum absolute coordinate delta
  at most 0.01 and minimum cosine similarity at least 0.9999. Maximum relative
  error is still measured but is diagnostic only because near-zero coordinates
  make it unbounded and operationally misleading.
- Every vector is dimension/finite validated, written to a same-directory
  temporary `.npy`, read-verified, and atomically replaced. Leaf and summary
  matrices, aligned ID arrays, and metadata use the same verified atomic
  publication discipline, and obsolete summary vectors are pruned only after
  the replacement matrix is durable.

Verification: a read-only representative benchmark over 512 existing leaf
payload shadows used 34 occupied buckets and 7,898 padding tokens over 221,990
real input tokens. End-to-end embedding improved from 28.1032s to 11.6945s
(2.40x); maximum absolute delta was 0.007135 and minimum cosine similarity was
0.999919. Synthetic fixtures prove separate queues, bad-peer bisection, no
partial failed-entity cache entry, successful-peer durability, retry
convergence, aligned matrices, and unchanged-run behavior. The full native
module loop and `./pocket-advisor.py test` pass 14/14; `git diff --check` is
clean. The benchmark was local and read-only; tests mutated only temporary
fixtures.

Deferred: none from Workstream B. The execution-recipe fingerprint deliberately
causes the next embed for each workspace to build a new vector cache while
leaving the prior inactive cache untouched. Content-addressed PDF transforms
and bounded concurrency are the new roadmap head.

## 2026-07-19 — One-shot and hierarchical thread summaries

Implementation commit: `6404eaa` (Workstream A in
`docs/ingestion/ingestion-performance.md`).

- Replaced sequential per-message rolling recompression with one prompt-v2
  generation over an explicitly delimited complete chronological thread at or
  below a measured 48,000-token one-shot quality ceiling. The prompt gives
  beginning, middle, and end facts equal weight and remains bounded, greedy,
   local, non-source, and independently durable per completed thread.
- Added the deterministic long-thread path: complete messages pack into
  24,000-token structural segments, segment summaries reduce chronologically
  through a fixed 16-way tree, and every reducer input stays below the measured
  quality ceiling. Only a single structurally oversized message may use the
  explicit tokenizer-aware fallback; its exact text is reconstructable from
  the slices, so no content is silently truncated.
- Bumped `SUMMARY_PROMPT_VERSION` to 2 so existing summaries and their vectors
  converge through the existing stale-summary mechanism. Retired the obsolete
  character-count `thread_summary_segment_chars` setting and made that key a
  configuration error.
- Made telemetry distinguish one-shot/hierarchical assignments, source
  segments, generation calls, reduction calls, input tokens, and the new
  8k/48k/unbounded length tiers. Progress now heartbeats an active first thread
  without falsely counting it complete.

Verification: a local synthetic MLX benchmark at 48,177 input tokens completed
in 76.1 seconds and retained all six semantic probes spanning the beginning,
middle, and end. The native fixture proves the three-segment plus one-reduction
path, exact deterministic oversized-message reconstruction, prompt-version
publication, and three `THREAD(sum)` positional retrieval expectations. All 14
native tests and `./pocket-advisor.py test` pass; `git diff --check` clean. No
live content or workspace state was read or mutated by tests or benchmarks.

Deferred: none from Workstream A. Shape-stable embedding microbatches are the
new roadmap head.

## 2026-07-19 — Ingest performance telemetry and benchmark baseline

Implementation commit: `eb8771e` (ingest-performance telemetry item; locked
contract in `docs/ingestion/ingestion-performance.md` and
`docs/ingestion/ingest-all-reporting.md`).

- Added `modules/telemetry.py`: one typed PerformanceTelemetry run recorder
  with explicit `measured`/`not_applicable`/`partial`/`not_run` states for the
  summaries, embed, and PDF stages. The CLI creates it before orchestration,
  injects it through the pipeline context, marks entry as `partial`, seals
  success as `measured`, and preserves stage-recorded deliberate gates, so
  aggregate telemetry survives any stage failure.
- Instrumented the current hot-stage implementations: rolling summaries record
  thread/message/segment/call counts, real tokenizer input tokens, fixed
  8192-token length tiers, and render/model/publication timings; embedding
  records separate leaf and summary queues with pending/successful/failed
  entities, input tokens, cache publications, and model/publication/assembly
  timings; the PDF stage records considered/pending occurrences, transform
  outcomes, verified-original fallbacks, worker/jobs/CPU-budget topology, and
  wall/OCR/text subphase timings.
- Cut saved ingest records to schema version 2 with the required nested
  `performance` block, typo-strict loading (unknown, missing, negative,
  non-finite, and irreconcilable values rejected; gated and never-entered
  stages must be all-zero), one compact terminal line per hot stage, and
  identical re-rendering through `ingest report`. Version-1 records remain on
  disk but are deliberately not loadable.
- Established the comparison baseline mechanism for the three optimization
  items: every full ingest now records its own reproducible aggregate
  baseline, the feature document retains the measured large/small-profile
  stage tables, and each workstream's tuning benchmarks land with that
  workstream as locked.

Verification: native suite 13/13 including new strict-contract,
round-trip, partial-survival, and state-distinction fixtures. A live
`ingest all` on the isolated test workspace converged the 27 PDF occurrences
onto the current extraction recipe and recorded a measured PDF stage (86.6s
OCR vs 0.5s text subphases within an 87s transform wall); the immediate
rerun completed unchanged in 0.6s with explicit measured-zero telemetry.
Saved records strict-load and re-render byte-identically; `git diff --check`
clean. No collection content or non-derived state was touched.

## 2026-07-19 — Post-ingest integrity and reporting fixes

Implementation commit: `5fd5bdd` (investigation record: `b678f14`,
`docs/bugs/post-ingest-integrity-reporting.md`).

- Bumped the Westpac parser to `westpac-v2` and recognized explicitly labelled
  compact account lines such as `Account No. 037-186 40-5530`, while retaining
  the existing two-column format, digit normalization, and loud configured-
  account mismatch behavior.
- Made completion reporting suppress transaction run flags only when their
  severity counts exactly equal the corresponding structured manifest finding
  totals. Non-equivalent flags remain visible as fallback signals.
- Made native verification distinguish recursively attached-email candidates
  from collection-root originals only after proving integrity membership, email
  type, an acyclic existing parent chain, and a terminal mounted blob-indexed
  carrying original. Added the verified attached-lineage count to the report.
- Added temporary-fixture regressions for both Westpac account layouts,
  equivalent and non-equivalent finding totals, valid attached-email integrity,
  missing lineage, an unindexed carrying root, and cyclic parents.

Verification: `./pocket-advisor.py test` passed 13/13; Python compilation and
`git diff --check` were clean. Read-only native verification of the existing
family workspace passed with 1,008 indexed originals, 1,027 memberships, 19
attached-email lineages, 3,691 derived artifacts, 10,541 leaf vectors, 126
navigation vectors, and a valid transaction manifest. No live derived state or
collection content was mutated.

Operational note: the next normal family-workspace `ingest all` performs one
fast Stage 5 rebuild because `westpac-v2` changes the parser fingerprint. No
PDF, summary, embedding, schema, or wipe work is required.

Deferred: the 121 unsupported AMP, MEBank, NAB, CBA, Revolut, and Qantas
statements remain the transaction-parser-coverage roadmap item. Genuine
ambiguous transfer candidates remain operator reconciliation findings.

## 2026-07-19 — Multiple-ingestion regression fixes

Implementation commit: `a9c9d96` (investigation and resolution record:
`docs/bugs/multiple-ingestion-errors.md`).

- Made Stage 3 fall back to the write-verified original PDF when OCRmyPDF
  refuses to produce a derivative. The authoritative acceptance gate remains a
  successful `pdftotext -layout` extraction, while the OCR refusal remains a
  review warning. Bumped the PDF-text recipe so prior artifacts converge once
  through the new contract.
- Corrected generic statement assertion discovery to bind the first decimal
  monetary value after its recognized label, preventing a later loan-limit
  value from masquerading as the opening balance. Zero is now preserved in
  conflict diagnostics.
- Made assertion-bearing zero-activity statements valid Stage 5 output with
  zero transaction rows, and bumped the shared transaction recipe so prior
  builds are safely invalidated.
- Corrected top-level source totals to derive from the integrity blob index and
  exclude recursively discovered attached emails. Split PDF extraction
  failures, OCR-recovery warnings, and weak-date warnings into distinct
  categories without equivalent run-flag duplication.
- Limited saved failed-stage reasons to aggregate-safe exception type and
  allowlisted structural conflict context, avoiding both opaque
  `ParserConflict` records and serialized corpus narrative.

Verification: every `modules/tests/test_*.py` fixture and
`./pocket-advisor.py test` passed (13/13); `git diff --check` clean. Tests used
synthetic temporary fixtures, and no corpus or live workspace state was
modified.

Operational note: the next `ingest all` reprocesses successful PDF occurrences
once under `pdf-text-v2`, then rebuilds transactions once under
`transactions-v2`; no state wipe is required.

Deferred: live-corpus acceptance is performed by that next operator-run
ingest. Parser support for the 121 statements from currently unsupported
institutions remains the transaction-parser-coverage roadmap item and was
deliberately not folded into this regression fix.

## 2026-07-18 — Transaction-stage convergence

Implementation commit: `aedd667` (locked design: `892a3bb`,
`docs/ingestion/transaction-stage-convergence.md`).

- Added a versioned Stage 3 PDF-text recipe fingerprint covering the local
  OCRmyPDF/`pdftotext` wrapper contract, OCR languages, and tool versions.
  Successful artifacts from an older recipe are now regenerated before
  transaction parsing; named Stage 5 execution fails safely when that
  prerequisite is stale.
- Added an atomic, write-verified workspace-local transaction build manifest
  with semantic input and canonical live-output digests, row cardinalities,
  and current aggregate findings. Extracted statement-text bytes are a direct
  input, so changed OCR/text output invalidates parsing even when the original
  PDF hash is unchanged.
- Made verified unchanged transaction runs skip statement detection/parsing,
  table rewrites, transfer matching, per-statement output, and duplicate
  review-log writes. Missing/corrupt manifests, parser/config/reconciliation
  changes, live-row divergence, and explicit force retain the existing full
  atomic rebuild.
- Added guarded `ingest transactions --force`, manifest-aware ingest/report
  findings and verification, exact current account/owner/holder convergence,
  and one-time graph cleanup when the final bank collection is unmounted.
- Added temporary-fixture coverage for Stage 3 recipe reprocessing, every core
   Stage 5 invalidation path, output drift, parser-set changes, manifest
  publication failure, persisted findings, CLI restrictions, final unmount,
  and cross-workspace manifest isolation.

Verification: all 13 native module tests and `./pocket-advisor.py test` pass;
Python compilation and `git diff --check` clean. No corpus or live workspace
state was modified. The first post-upgrade full ingest may reprocess existing
PDF text once to establish the new recipe fingerprint, then performs one
transaction rebuild to publish its initial manifest.

Deferred: broader institution parser coverage remains its own independent
roadmap item and does not block the local answering pass.

## 2026-07-18 — PDF OCR validation-warning recovery

Implementation commit: `838e037`.

- Made `pdftotext -layout` the final readability gate when OCRmyPDF completes
  with a non-zero status after producing a fresh derivative. Success requires
  a zero `pdftotext` exit, a present output file, and a readable text artifact;
  the OCR diagnostic remains a review warning.
- Removed stale OCR/text outputs before every attempt so an earlier derivative
  cannot masquerade as current, made prior `extraction_method='error'` rows
  retryable, and cleared obsolete failure details after successful recovery.
- Preserved combined OCR and text diagnostics when the final extraction still
  fails, while keeping successful warning-bearing PDFs searchable and visible
  in the run-local finding summary.
- Extended the isolated PDF fixture across the warning-success, hard-failure,
  missing-output, retry, and post-recovery idempotence paths; locked the final
  acceptance rule in `docs/design.md`.

Verification: native suite 13/13; Python compilation and `git diff --check`
clean. A read-only-source retry against the isolated test workspace recovered
both occurrences of one malformed PDF, produced ten leaf chunks, converged at
27/27 readable PDF occurrences and 553 leaf vectors, and was a no-op on the
next full ingest. Originals were not modified.

Deferred: none.

## 2026-07-18 — Flat workspace-state layout

Implementation commit: `b6b0391` (locked design:
`docs/storage/workspace-scoped-state.md`; accuracy refinement:
`docs/benchmarks/accuracy-testing.md`).

- Flattened each workspace container from
  `.state/workspaces/<workspace_id>/` to
  `.state/workspace-<workspace_id>/`, removing the redundant intermediate
  directory.
- Replaced the generic `pocket_advisor.db` filename with
  `<workspace_id>.db`, making the database's filesystem identity agree with
  its schema-bound workspace owner.
- Moved retrieval expectations and result history from the registry workspace
  root into the plural
  `<workspace-state>/search-accuracy-tests/` directory and made all accuracy
  actions resolve it from the bound `Config` with symlink refusal.
- Refined `wipe state` to delete only regenerable children while preserving
  the complete human-authored accuracy suite and result history
  byte-identically. Confirmation, daemon shutdown, exact containment,
  protected-root, and cross-workspace isolation safeguards remain intact.
- Updated the holistic/feature designs, embedding path, runbook, and safety
  instructions; added exact path, DB name, suite location, symlink, and wipe
  preservation fixtures.

Verification: native suite 13/13; Python compilation and `git diff --check`
clean. No live workspace state, accuracy files, or content was moved, copied,
deleted, initialized, or migrated.

Operational note: earlier nested state roots and workspace-root
`search-accuracy-test/` directories are intentionally not auto-migrated.
Engine state is rebuilt in the flat layout; an operator explicitly relocates
any human-authored expectation sets that should be retained.

## 2026-07-18 — Retired shared-layout state removed

Operational milestone (no implementation commit).

- The operator manually deleted the retired shared cache and database at
  `workspaces/.state/cache/` and
  `workspaces/.state/pocket_advisor.db`.
- Both exact paths were subsequently verified absent. No agent deletion or
  workspace-state mutation was performed.
- Native `wipe state` remains intentionally scoped to one explicitly selected
  workspace; no `wipe legacy` command was added because there is no remaining
  legacy target to service.

Deferred: none. This cleanup is removed from the roadmap; transaction-parser
coverage remains independent future work.

## 2026-07-18 — Quoted-reply duplicate-prefix compaction fix

Implementation commit: `99cc7b9`.

- Fixed a conservative false negative where the resolved direct parent's
  initial 16-token prefix occurred both in its direct quote and again in
  nested quoted history, causing the complete child body to remain indexed.
- Bumped the detector to version 6. A repeated minimum prefix may now resolve
  only when an exact 64-token confirmation uniquely selects the earliest
  candidate; a later nested match is never selected after the earliest
  candidate diverges.
- Preserved the existing hard boundaries: exact RFC parent identity, no fuzzy
  text matching, and client wrapper recognition that can expand but never
  authorize a cut.
- Added a durable reproduced-finding record under `docs/bugs/`, updated the
  holistic design, and added regressions for resolvable nested repetition,
  misleading later matches, and genuinely ambiguous duplicates.

Verification: the existing affected artifacts resolve read-only at the
correct Gmail wrapper (13,972 redundant characters identified); native suite
13/13; Python compilation and `git diff --check` clean. No workspace state or
original content was changed.

Operational note: an already-chunked workspace requires its normal explicitly
confirmed state wipe and complete re-ingestion to apply a changed authored
body; the stale-chunk guard deliberately refuses an in-place rewrite.

## 2026-07-18 — Adapter retirement

Implementation commit: `4037db7` (locked daemon design:
`docs/retrieval/query-daemon.md`).

- Replaced every remaining frozen-adapter command with native workspace-safe
  implementations: session-warm relational query daemon, full integrity/index
  verification (including both FTS5 integrity checks), indexed-blob lookup,
  and vector-index list/wipe maintenance.
- Made query and accuracy reuse one warm retrieval-resource bundle while
  preserving explicit cold fallback and one native retrieval implementation.
- Added exact workspace, containment, symlink, active-index confirmation, and
  daemon-shutdown safety boundaries for maintenance and runtime paths.
- Moved all runtime dependencies and strict defaults to repository-root
  configuration, removed obsolete venv packages, and deleted the complete
  retired `scripts/` implementation and test tree.
- Updated CLI/operator/architecture documentation and added native daemon,
  maintenance, CLI, accuracy, and workspace-isolation coverage.

Verification: native suite 13/13; Python compilation, `git diff --check`, and
`pip check` clean; read-only daemon status, index listing, and blob-source
listing exercised against workspace-scoped state. No content or workspace
derived state was changed.

Deferred: none from Adapter retirement. Transaction-parser coverage remains
independent roadmap work. Retired shared-state cleanup was subsequently
completed manually by the operator.

## 2026-07-18 — Native retrieval-expectation accuracy suite

Implementation commit: `3d8d9d7` (locked design:
`docs/benchmarks/accuracy-testing.md`). Ships the accuracy portion of the
adapter-retirement phase ahead of schedule.

- Added workspace-generic, workspace-bound `accuracy
  generate/run/compare/list` in `modules/accuracy.py`; sets and results
  live under `<workspace-root>/search-accuracy-test/` as gitignored
  workspace data.
- `generate` scaffolds anchor-verified entries (summarized threads,
  documents) with TODO questions for human authoring; `--force` guards
  overwrites.
- `run` executes warm through the real retriever and writes a
  schema-versioned, write-verified JSON record per run: per-question
  verdict (STRONG / THREAD(sum) / THREAD / MISS / INVALID / SKIPPED),
  rank, matched anchor, latency, aggregates, and an environment block
  (embed fingerprint, rerank model, top-k, corpus counts,
  expectation-set SHA); exits non-zero on MISS or INVALID.
- `compare --last N` reports per-run aggregates, every per-question
  verdict/rank change, and expectation-set drift; replaces the planned
  file-addressed workspace-free compare.
- Retired the "golden set" naming across live docs in favour of
  "retrieval expectation set"; durable anchors only (Message-IDs, thread
  stable keys).

Verification: an isolated 12-question expectation set passes 12/12 (100%
thread-or-better) through the real models, with all four cross-lingual
questions at rank 1. Module suite 11/11 with the new fixture; frozen suite
untouched; `git diff --check` clean.

Subsequently completed: frozen accuracy-code deletion shipped with Adapter
retirement (`4037db7`). The `envelope-v1` versus plain-payload A/B remains an
experiment.

## 2026-07-18 — Generic end-to-end platform rehearsal completed

Operational milestone (no implementation commit); scope reduction
recorded at `8d9a1ae`; run record `20260718T050815083153Z`.

- Ran a from-scratch `ingest all` over the extended test corpus — 60
  originals (56 emails including a newly added Russian thread, 4 native
  PDFs) — through the final schema, envelope-enriched payloads, thread
  summaries, dual vector index, transactions, and the completion report.
- Completed in 11m06s: 554 leaf + 7 navigation vectors (indexes
  consistent), 7/7 eligible summaries generated, 4 Westpac statements /
  1488 rows balance-ok, 2 tolerated OCR failures correctly reported as
  findings with review-queue entries.
- Established reusable performance rates: OCR ~3.3 s/PDF, summaries
  ~8.1 s/message, and embedding ~0.27 s/chunk.
- Established a complete, isolated rebuild/report/retrieval-validation
  workflow that does not depend on any particular live workspace.

Deferred: broader statement-parser coverage remains an independent operational
follow-up. Retired shared-layout state was subsequently removed manually by
the operator.

## 2026-07-18 — Full-ingest completion reporting and saved-record display

Implementation commit: `78e705a`.

- Every `ingest all` run now ends with the typed completion report:
  monotonic per-stage/pipeline timings with run-local `StageStats`, a
  read-only workspace snapshot (sources, content, PDFs, threads/summaries,
  search indexes, transactions), and severity-graded finding rollups.
- Wrote an aggregate-only, schema-versioned, atomically write-verified JSON
  record per run under the selected workspace's `logs/ingest-runs/`;
  `INCOMPLETE`/`REPORT FAILED` semantics preserve the original pipeline
  error and exit non-zero when the reporting contract is unmet.
- Shared the transaction coverage classifier with `transactions report`,
  classifying single-account unmatched transfer-like debits honestly as
  `single_account_unverifiable` instead of “all accounts covered”.
- Added `ingest report [--last | PATH]` (feature decision 13): loads a
  persisted record back through the shared formatter for an identical later
  rendering; latest resolves by filename ordering with no symlink; the
  command opens no database and rejects conflicting, missing, or
  wrong-schema inputs with clear messages.
- Documented the records location and display command in the RUNBOOK and
  the feature doc (decision 13, acceptance criterion 14).

Verification: native module tests 10/10 (including the new reporting and
CLI-display fixtures); frozen tests 11/11; `git diff --check` clean;
`ingest report --last` verified end-to-end against a real saved record in
an isolated validation workspace. No live corpus or workspace state was
touched.

Subsequently completed: native integrity/FTS integrity verification shipped with
Adapter retirement (`4037db7`).

## 2026-07-18 — Command-scoped workspace selection

Implementation commit: `c6df0a3`.

- Enforced workspace selection after parsing the complete command/action,
  requiring it for every database, ingestion, retrieval, daemon, wipe,
  integrity, verification, transaction, and workspace-owned accuracy action.
- Made shared `fetch-model` and fixture `test` registry-free and
  workspace-free; preserved explicitly file-addressed `accuracy compare` as
  workspace-free while its native port remains fail-closed.
- Rejected meaningless `--workspace` selectors on workspace-free actions and
  kept help at every parser level state-free.
- Updated model-fetch guidance, runbook/verification commands, and exhaustive
  CLI matrix coverage for scope, registry bypass, error behavior, and nested
  help.

Verification: workspace-free native test command 9/9; frozen tests 11/11;
Python compilation and `git diff --check` clean. No live workspace state was
touched.

Subsequently completed: native accuracy shipped at `3d8d9d7`; the remaining
frozen-command retirement shipped at `4037db7`.

## 2026-07-18 — Workspace-scoped state and mandatory workspace selection

Implementation commit: `23b0a42`.

- Made global `--workspace <id>` mandatory for every operational command and
  carried the selected workspace explicitly through pipeline and retrieval
  context; removed the redundant `active:` registry key entirely.
- Isolated each workspace's bound SQLite database, cache, vector indexes,
  logs, and runtime files under
  `workspaces/.state/workspaces/<workspace_id>/`, with only model weights
  shared.
- Added database ownership metadata, legacy/mismatch refusal before schema
  mutation, and foreign-workspace integrity-row checks.
- Added exact, confirmed workspace-state deletion with protected-root and
  symlink defences; frozen commands that cannot honor workspace isolation now
  fail closed rather than entering shared-state adapters.
- Added two-workspace fixtures covering shared and distinct mounts, identical
  Message-IDs, independent FTS/vector state, transaction rebuild isolation,
  wipe cancellation/deletion, byte-level non-interference, and redirected
  state refusal.

Verification: native module tests 9/9 through the mandatory-workspace CLI;
frozen tests 11/11; Python compilation and `git diff --check` clean. No live
workspace state was initialized, wiped, or ingested.

Subsequently completed: accuracy shipped at `3d8d9d7`; daemon, verify,
blob-index lookup, vector-index wipe, and frozen-tree deletion shipped at
`4037db7`. Operator-chosen workspace rebuilds remain outside the platform
roadmap and require immediate confirmation before a scoped wipe.

## 2026-07-18 — Envelope-enriched payload + message-artifact consolidation

Implementation commit: `a48bf7b`.

- Consolidated each email cache to write-verified
  `email_message_full.txt` and `email_message.txt` artifacts.
- Made the authored body region of `email_message.txt` the email leaf-chunk
   source, with envelope-relative offsets and pure source `chunks.text`.
- Added source-aware email, attachment, and native-document retrieval payloads
  shared identically by dense embedding and `chunks_fts.payload_shadow`.
- Added the `envelope-v1` payload recipe to the embedding fingerprint so a
  recipe change selects a new vector cache without re-chunking.
- Added fresh-schema refusal for pre-payload chunk/FTS layouts and fixture
  coverage for payload derivation, FTS envelope hits, fingerprint separation,
  pure snippets, offsets, and the final two-artifact cache.

Verification: native module tests 8/8; frozen tests 11/11; `git diff --check`
clean. No live corpus or derived state was touched.

Deferred: measure enriched versus plain payload retrieval after the native
accuracy runner is ported; adoption of the enriched recipe is already locked.

---

## Pre-rewrite history (2026-07-10 – 2026-07-17)

The following milestones are from the original engine implementation, which
was fully rewritten under `modules/` starting 2026-07-18.

**2026-07-17 — Layout-preserving OCRmyPDF extraction**: all PDF and image
text now follows one OCRmyPDF `--redo-ocr` → `pdftotext -layout` sequence.
The former direct image OCR binding, confidence threshold, and review-copy
configuration were removed. Full-page image-vector retrieval was retired
after benchmarking showed no retrieval benefit.

**2026-07-16 — Single-entrypoint CLI + workspace wipe**: new
`./pocket-advisor.py` CLI owns all argument parsing (the only argparse in
the codebase). `wipe state` provides confirmed derived-state deletion for
from-scratch re-ingests. Interactive progress reporting added for long
pipeline loops.

**2026-07-15 — Structured transactions v2**: replaced regex heuristic with
a real bank-statement pipeline (`holders`/`accounts`/`statements`/
`transactions` schema, signed minor units, per-format parsers, assertion
validation, transfer matching, coverage reporting). First live Westpac
parser shipped.

**2026-07-14 — Multi-model vector cache + MLX-only stack**: per-model
vector cache directories so model switching never deletes another model's
cache. Unified MLX loader for embed and rerank; GGUF/llama.cpp backend
removed. `eval.py` renamed to `search_accuracy_test.py`.

**2026-07-13 — Schema evolution + purpose-scoped mounts**: three schema
iterations (A: collection-scoped identity, B: items+memberships rename,
C: polish) established the relational foundation. Purpose-scoped mount
filtering shipped. Warm-mode accuracy testing and session-warm query
daemon landed.

**2026-07-12 — Pocket Advisor rename + pluggable backends**:
repo/branding renamed from `pocket-lawyer` to `pocket-advisor`. Pluggable
embedding backends (`llama_cpp` | `mlx`) with fingerprint wipe-on-change.
Search-accuracy-test harness, pre-filter, reranker, and transliteration
shipped.

**2026-07-11 — Standalone document ingestion**: non-`.eml` documents
ingested as synthetic email parents with a `documents` table and
`date_source` tracking.

**2026-07-10 — Core pipeline**: parse → attachments/OCR → JWZ threading
→ chunk/embed → hybrid query (FTS + dense + RRF). End-to-end on live
corpus.
