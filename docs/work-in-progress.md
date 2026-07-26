# Work in Progress

Scratch pad for the feature currently being implemented. When idle, this file
is intentionally near-empty.

When a roadmap item is picked up, move it here with any active context needed
to resume work. When the item is done, its final state is locked down in the
feature design doc, added to `docs/changelog.md`, and removed from this file.

See the lifecycle workflow in `AGENTS.md`.

## Current work items

### Structured execution logging (picked up 2026-07-26)

Design: `docs/platform/logging.md` — **locked for implementation
2026-07-26** (two review rounds folded in). Roadmap item 1. Read the design
doc first; this section carries only the active context.

Progress: steps 1–3 done (facade, progress observer hook, entrypoint
wiring with banner/footer + `PipelineContext.log`). Steps 4–8 not started.
16/16 suite green.

Four review findings reshaped the first draft — do not resurrect the
originals:

1. **`ctx.log` alone can't reach the code that matters.**
   `modules/inference.py:59` takes `config`, not `ctx`, and that is where
   the motivating failure originated. Hence D6: process-scoped
   `get_log()`, configured once by `setup_logging()` at the CLI
   entrypoint, with `ctx.log` as a convenience alias.
2. **stdlib `logging` is the engine** (D1), not a bespoke writer — the
   API stays the three requested methods, but this buys `httpx`/`httpcore`
   DEBUG capture (which explains the `RemoteProtocolError`), `exc_info`,
   and `QueueHandler` thread-safety.
3. **`"YYYYMMDD HH:MM:SS"` is not ingestible** — no timezone, not
   RFC 3339, second resolution loses ordering across the 10
   `pdf-transform` workers. Superseded by finding 6 below: one RFC 3339
   UTC-millis `timestamp` field (D5a).
4. **Repo-root `logs/` was dropped** for
   `workspaces/.state/workspace-<id>/execution-logs/` (D7) — these records
   carry case data (the motivating transcript logs an account number), and
   the `workspaces/` blanket ignore should cover them rather than one new
   `.gitignore` line. Survival across `wipe state` is one line:
   `"execution-logs"` into `PRESERVED_STATE_NAMES` (`modules/wipe.py:12`).

A second revision the same day unified the interactive channel into the
same facade and collapsed the duplicated fields:

5. **`.interactive()` is now the fourth method** and the facade also
   constructs the progress bars (`log.progress()`, `log.worker_pool()`) —
   nine call sites across seven modules stop importing
   `modules/progress.py`. Destination is a property of the method:
   `.interactive()` and `.error()` reach the terminal, `.info()` and
   `.debug()` are file-only. **`.info()` being file-only is a deliberate
   refinement** of the original sketch — it is what lets instrumentation
   grow without degrading the terminal.
6. **Six schema fields, no duplicates**: single `timestamp` (RFC 3339,
   UTC, millis — universal across OpenObserve/ES/Loki/Splunk/Datadog),
   single `worker_thread` (thread *name*, since `get_ident()` is reused
   after thread death and would be ambiguous within one run), and
   `logger` renamed to **`caller`**.
7. **`run_id` appears in the terminal as a start banner + end footer**,
   not per-line — 36 chars of UUID on every line would wreck the readable
   output and break progress-bar width. Footer prints on failure and
   Ctrl+C too.

Two landmines while implementing:

- **Progress-bar corruption (D3).** A bare `print()` inside the logger
  will shred an active redraw — `Progress.println()`
  (`modules/progress.py:89-96`) exists for exactly this. The facade
  constructing the bars is what makes routing automatic; bars emit one
  lifecycle record on `done()`, never per-redraw.
- **Name collision.** `Config.logs_dir` (`workspace-<id>/logs/`) is the
  *state* logs — `review_queue.csv`, `ingest-runs/`, wiped with state. The
  new `execution-logs/` is a distinct sibling, preserved. A subdirectory
  of `logs/` would have made the one-line preserve-rule impossible.

Query daemon (`modules/daemon.py:222 serve()`) is explicitly out of scope
— it needs a session-plus-request id design of its own.

Implementation order is the design doc's 8 steps (logs.py + tests →
progress registration → entrypoint wiring → wipe preservation → report
`run_id` correlation with `REPORT_SCHEMA_VERSION` 4→5 → call-site
migration → third-party capture → verification).
