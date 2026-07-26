# Live Ingestion Pipeline Terminal Dashboard

Status: **implemented 2026-07-26**. Initial design commit `32aba8f`;
implementation commit `4d7021a`; locked implementation described below.
Persistent-final-state and single-interrupt amendment in progress 2026-07-26.

Implementation map:

- `modules/runtime_dashboard.py` — run/stage/event model and Rich renderer;
- `modules/cli.py` — full-ingest lifetime and stage transitions;
- `modules/progress.py` — dashboard snapshots for task, worker-pool, and
  inference-queue widgets;
- `modules/logs.py` — terminal-event routing while structured records continue;
- `modules/tests/test_runtime_dashboard.py` — render, routing, proxy, and
  fallback coverage.

## Goal

Build one comprehensive Python Rich dashboard for:

```bash
uv run pocket-advisor.py --workspace test ingest all
```

This is a terminal UI and operator-experience concern. Structured execution
logging remains a separate durable observability concern.

The dashboard must make a long run feel alive, explain what the engine is
doing now, preserve honest measures of progress, surface failures without
destroying the display, and hand off cleanly to the existing final ingest
report.

## What the pipeline actually does

The dashboard represents the real CLI-owned stage order rather than inventing
a simplified job model:

1. **Discover** walks every mounted collection once, hashes originals, updates
   ingestion candidates, and refreshes the source blob index.
2. **Emails** parses candidate MIME messages and recursively processes attached
   messages/ZIP members, renders verified artifacts, derives authored bodies,
   creates chunks, and begins asynchronous leaf embedding as content becomes
   ready.
3. **PDFs** registers native and attached PDF occurrences, decides which unique
   transforms are pending, runs bounded OCR/text-extraction workers, publishes
   verified text, creates chunks, and begins asynchronous leaf embedding.
4. **Threads** reconstructs email conversations and direct-reply edges.
5. **Summaries** always performs staleness maintenance, optionally generates
   eligible thread summaries through bounded inference workers, publishes
   verified summary artifacts, and dispatches summary embeddings.
6. **Embed** first settles readiness work, then converges every remaining
   leaf/summary gap and publishes the two vector indexes.
7. **Transactions** either skips honestly when no bank-statement collection or
   prior transaction state exists, converges an unchanged graph, or performs
   the deterministic atomic statement rebuild and reconciliation pass.

Several denominators are discovered only after an earlier stage runs.
Embedding submissions also grow throughout producer stages. Therefore the UI
must never present one fabricated item-level percentage or ETA for the whole
pipeline. It may truthfully show stage completion (for example, 3 of 7 stages)
and task-local percentages/ETAs where a real total exists.

## User experience

### One live surface

On an interactive terminal, Rich `Live` owns one bounded region. It contains:

- a title with workspace, run id, total elapsed time, and overall run state;
- a seven-row pipeline table showing pending/running/completed/skipped/failed
  state, per-stage elapsed time, and the completed stage's aggregate result;
- an active-work panel containing the current single-task progress, PDF worker
  slots, and persistent inference queue pressure;
- a small, bounded recent-events panel for warnings and useful stage messages;
- a footer explaining the symbols and showing that the full execution record
  is still being written.

The dashboard uses colour and glyphs as redundant cues, never as the only
source of meaning. Status words remain visible. Long filenames and aggregate
summaries are clipped to the current terminal width by Rich rather than
producing horizontal terminal damage.

The display refreshes at four frames per second. Task state can update more
often without forcing a terminal write on every item.

### Honest indicators

- The header progress bar is explicitly **stages**, not percent of workload.
- A running stage has a spinner and elapsed time.
- Completed stages retain their duration and compact `StageStats` result.
- Skipped stages show the configuration/data reason.
- After a failure, the failing stage and all `not_run` stages remain distinct.
- A determinate task shows count, percent, recent rate, and ETA.
- An indeterminate task shows count, elapsed time, and current item.
- The PDF pool shows aggregate progress plus one row per worker with its active
  document and per-job elapsed time.
- An inference queue shows queued, in-flight, completed, failed, and pending
  counts. It deliberately has no percentage or ETA while producers can still
  add work.
- Recent events are capped. The durable JSONL log, not terminal history, is the
  complete event record.

### Completion and failure

Rich owns the complete interactive `ingest all` lifetime. The display is
non-transient: after the pipeline and read-only report audit finish, the last
frame remains above the returned shell prompt. That final frame retains the
seven pipeline rows and replaces active-work detail with the report's compact
performance, workspace-snapshot, findings, review/report path, and execution
log information. No second plain-text report, banner, or log footer is printed
around it.

This follows Rich 14.1.0's documented `Live` contract: the final frame of a
non-transient display remains when `stop()` returns, and an oversized final
frame is rendered as `visible` once clearing is no longer required. The live
phase remains cropped to the terminal height.

On pipeline failure or `KeyboardInterrupt`, the dashboard paints
failed/not-run or interrupted state, performs only bounded coordinator
cleanup, publishes any safe aggregate report it can build, and stops Rich
exactly once. The first Ctrl+C sets the process interruption flag, terminates
active OCR process groups, cancels queued inference jobs, and allows in-flight
inference worker threads to be abandoned as daemon workers. They cannot hold
interpreter shutdown behind a remote HTTP timeout. Exit code remains 130, with
no traceback, cursor damage, or second interrupt required.

## Architecture

### D1. A UI module, not a logging rewrite

`modules/runtime_dashboard.py` owns the run-scoped Rich presentation model.
`modules/logs.py` continues to own JSONL setup, record schema, level policy,
third-party capture, and lifecycle. The dashboard neither reads nor tails the
JSON log.

The old `.interactive()` logging method is retired. Operator notices use
`.notice()`: the event is recorded once, while terminal presentation goes
through Rich—into the active dashboard's bounded event panel, or through a
plain-capable Rich `Console` when no dashboard owns the command. `.error()`
uses the same Rich presentation boundary and remains an error-level structured
record. File-only `.info()` and `.debug()` remain invisible.

The final report is not sent through notice logging. The typed report is
rendered by the dashboard and recorded file-only at aggregate granularity;
non-TTY fallback uses the existing plain formatter. This prevents a multiline
report from being reinterpreted as recent events or printed above the live
region.

Stage summaries are represented primarily in the pipeline table. They may also
appear briefly as recent events; this is useful feedback, not the durable
record.

### D2. The CLI owns dashboard lifetime and stage state

Only `run_ingest(stage="all", ...)` starts the dashboard. The CLI already owns
stage sequencing, timing, skip gates, failure classification, and final report
assembly, so it is the only layer able to publish truthful pipeline-row state.
Stages remain UI-agnostic.

The dashboard starts before the database is opened, receives
`stage_started()` / `stage_finished()` transitions around `_execute_stage()`,
and remains active through `_finalize_ingest_report()`. The finalized typed
report is installed directly into the presentation model before `Live.stop()`.
Stop is idempotent so every return/unwind path can call it safely.

The generic CLI banner/footer are suppressed only when this Rich surface owns
interactive full-ingest output; their run/workspace/log identity is already
present in the dashboard. Non-TTY execution keeps the stable plain report and
correlation lines.

### D3. Existing progress objects become dashboard data sources

`Progress`, `WorkerPoolProgress`, and `QueuePanel` retain their public APIs and
non-interactive rendering. While a full-ingest dashboard is active, they
register as dashboard widgets and do not create their legacy ANSI
`LiveDisplay`.

Rich renders their current state. Worker threads only mutate widget state; the
Rich refresh thread is the only terminal painter. This removes competing
terminal ownership without making pipeline stages depend on Rich.

Rich `Live.start()` replaces the process's default stdout/stderr with its own
file proxies. Their TTY answer is not the original terminal's answer, so a
widget created with its default stream joins the already-active dashboard by
run scope rather than re-checking that proxy. An explicitly supplied stream
keeps its caller-requested legacy/plain renderer. This distinction is covered
against a real started `Live`, not only against a hand-built stream fake.

The existing `LiveDisplay` stays as the compatibility renderer for other TTY
commands in this first delivery. A later all-CLI dashboard project can retire
it deliberately after each command gets a purpose-built surface.

### D4. Activation and non-TTY contract

The dashboard activates only when both stdout and stderr are TTYs. This avoids
stealing notice output from a pipe merely because stderr happens to
be attached to a terminal.

When either stream is not a TTY:

- no `Live` object is constructed;
- progress widgets retain bounded plain-line output;
- `.notice()` uses a Rich `Console` on stdout and `.error()` uses one on
  stderr, both with terminal control disabled automatically;
- the command remains suitable for redirection, CI, and captured tests.

`TERM=dumb` and Rich's normal terminal capability detection disable colour
without changing content.

### D5. Dependency and failure isolation

Rich is the official Textualize package documented at
`https://rich.readthedocs.io/en/latest/introduction.html`, pinned to
`rich==14.1.0` — the exact version identified by those docs during
implementation — through `pyproject.toml` and `uv.lock`. The implementation
uses its public `Console`, `Live`, `Panel`, `Table`, `Text`, `Progress`,
progress-column, and `Spinner` APIs.

Presentation must not be able to invalidate ingestion. Dashboard construction
or rendering failure disables the dashboard and falls back to the existing
plain terminal behavior; pipeline state and exit semantics continue. Rich
shutdown is best-effort and idempotent.

## Verification

Automated tests must prove:

1. the seven stages begin pending and transition through running, completed,
   skipped, failed, and not-run states with honest timing/results;
2. `Progress`, `WorkerPoolProgress`, and `QueuePanel` register with the active
   dashboard instead of the legacy compositor;
3. notice/error messages become bounded events while their structured
   logging records remain unchanged;
4. non-TTY output and named-stage ingestion do not activate Rich;
5. a synthetic TTY render contains the workspace/run identity, pipeline,
   active work, queue pressure, recent event, and stage result;
6. a finalized report remains as the non-transient last Rich frame and no
   duplicate plain report/banner/footer is emitted;
7. one Ctrl+C abandons queued and in-flight-waiting inference work, closes
   Rich, and returns 130 without a traceback or second signal;
8. stop is idempotent and an exception leaves the terminal renderer closed;
9. existing progress, logging, CLI timing/failure-report, and dispatch tests
   remain green.

Before handoff, run the full repository verification required by `AGENTS.md`.

## Verification record

- All 20 native suites pass, including the new dashboard suite; the aggregate
  `uv run pocket-advisor.py test` command reports 20/20.
- A real PTY run of
  `uv run pocket-advisor.py --workspace test ingest all` completed in 1m07s
  with run id `0be09cd6-0fea-4572-972f-cc8770a0d042`.
- That run exercised the complete dashboard-to-report lifetime and a real
  failure path: four summary calls remained visibly in flight until oMLX
  closed their chunked responses; the dashboard surfaced the
  `RemoteProtocolError`, later stages continued, Rich restored the cursor and
  cleared its transient region, and the stable final report correctly rendered
  `INGEST COMPLETE WITH FINDINGS`.
- The live run exposed narrow-terminal wrapping in long recent events. The
  implemented renderer clips each event to one ellipsized Rich `Text` row; an
  80-column regression assertion locks that behavior.
