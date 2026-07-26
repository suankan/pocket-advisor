# Live Ingestion Pipeline Terminal Dashboard

Status: **locked for implementation 2026-07-26**.

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

The live surface stops before the existing final report is rendered. This
leaves the final report and exact report path as stable scrollback instead of
hiding them in a transient dashboard.

On pipeline failure the dashboard first paints the failed/not-run state, then
stops so the existing incomplete report and exception semantics remain intact.
`KeyboardInterrupt` and unexpected unwinds also stop Rich cleanly; no cursor or
alternate-screen state is left behind.

## Architecture

### D1. A UI module, not a logging rewrite

`modules/runtime_dashboard.py` owns the run-scoped Rich presentation model.
`modules/logs.py` continues to own JSONL setup, record schema, level policy,
third-party capture, and lifecycle. The dashboard neither reads nor tails the
JSON log.

When the dashboard is active, the logging facade's terminal-facing
`.interactive()` and `.error()` messages are offered to the dashboard's bounded
event panel. They are still recorded exactly once through the normal logging
path. File-only `.info()` and `.debug()` remain invisible. This small routing
seam prevents stdout/stderr writes from corrupting Rich without coupling log
records to UI frames.

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
and is stopped before `_finalize_ingest_report()`. Stop is idempotent so every
return/unwind path can call it safely.

### D3. Existing progress objects become dashboard data sources

`Progress`, `WorkerPoolProgress`, and `QueuePanel` retain their public APIs and
non-interactive rendering. While a full-ingest dashboard is active, they
register as dashboard widgets and do not create their legacy ANSI
`LiveDisplay`.

Rich renders their current state. Worker threads only mutate widget state; the
Rich refresh thread is the only terminal painter. This removes competing
terminal ownership without making pipeline stages depend on Rich.

The existing `LiveDisplay` stays as the compatibility renderer for other TTY
commands in this first delivery. A later all-CLI dashboard project can retire
it deliberately after each command gets a purpose-built surface.

### D4. Activation and non-TTY contract

The dashboard activates only when both stdout and stderr are TTYs. This avoids
stealing `.interactive()` stdout from a pipe merely because stderr happens to
be attached to a terminal.

When either stream is not a TTY:

- no `Live` object is constructed;
- progress widgets retain bounded plain-line output;
- `.interactive()` remains stdout and `.error()` remains stderr;
- the command remains suitable for redirection, CI, and captured tests.

`TERM=dumb` and Rich's normal terminal capability detection disable colour
without changing content.

### D5. Dependency and failure isolation

Rich is a normal runtime dependency, pinned through `pyproject.toml` and
`uv.lock`.

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
3. interactive/error messages become bounded events while their structured
   logging records remain unchanged;
4. non-TTY output and named-stage ingestion do not activate Rich;
5. a synthetic TTY render contains the workspace/run identity, pipeline,
   active work, queue pressure, recent event, and stage result;
6. stop is idempotent and an exception leaves the terminal renderer closed;
7. existing progress, logging, CLI timing/failure-report, and dispatch tests
   remain green.

Before handoff, run the full repository verification required by `AGENTS.md`.
