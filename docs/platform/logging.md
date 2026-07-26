# Structured Execution Logging

Status: **locked for implementation 2026-07-26** — roadmap item 1 in
`docs/roadmap.md`. Proposed 2026-07-26; revised the same day after design
review (reachability without `ctx`, stdlib-logging engine, ingestible
timestamps, case-data location), then again to unify the interactive
channel into the same facade and collapse the duplicated schema fields.
Active context lives in `docs/work-in-progress.md` while this is built.

## Problem

Every pipeline stage today talks to the operator through two ad hoc,
untyped channels: `print()` (stage summaries, warnings, errors — e.g.
`modules/pipeline/base.py:72`, `modules/pipeline/summaries.py:99`,
`modules/embedding/dispatch.py:77-84`) and `modules/progress.py`'s
`Progress`/`WorkerPoolProgress`, constructed directly by nine call sites
across seven modules. Both are text on stdout/stderr, human-shaped, and
gone the moment the terminal scrolls. There is no stdlib `logging` usage
anywhere in the codebase, no run-scoped identifier stitching one
invocation's output together, and no machine-readable step-by-step record
— only the coarse, aggregate-only end-of-run report
(`modules/ingest_report.py`).

The motivating incident: an `ingest all` where the summarisation endpoint
died mid-run (`RemoteProtocolError: peer closed connection without sending
complete message body`), leaving 4 threads un-summarized. Everything known
about *why* lived in terminal scrollback, and the failing layer
(`modules/inference.py`) produced one summary string with no request
context, no connection detail, and no stack.

## Scope

`modules/logs.py` becomes the **single entry point for all operator-facing
output** — structured records, terminal lines, and progress bars alike.
Every pipeline call site that prints or draws goes through it. A sweep of
the direct operator-invoked command prints in `modules/cli.py` (query,
accuracy, index-list, verification) is a non-goal; those are not pipeline
stages.

## Design decisions

### D1. `modules/logs.py`, a facade over stdlib `logging`

The module is named `logs.py`, not `logging.py`: the latter shadows the
stdlib module for any in-package `import logging` and confuses readers and
tooling. It follows the `modules/progress.py` convention — flat,
cross-cutting infrastructure.

The *engine* underneath is stdlib `logging`, an implementation detail
nothing outside `modules/logs.py` imports. This buys four things a bespoke
writer would have to re-implement or forgo:

- **Third-party diagnostics.** `httpx` and its `httpcore` transport already
  emit detailed DEBUG records — connection lifecycle, which request failed
  mid-body. That is precisely what the motivating incident lacked. Under
  `LOG_LEVEL=debug` these are captured into the same JSON stream,
  correlated by the same `run_id`, for free.
- **`exc_info`** — exception type, message, and formatted traceback.
- **Thread-safe, non-blocking writes** via `QueueHandler` + `QueueListener`.
- **Level gating** that is a genuine no-op below threshold.

Third-party capture is scoped deliberately: only `httpx` and `httpcore`
loggers are attached, at DEBUG, only under `LOG_LEVEL=debug`. The root
logger is not hijacked. A future dependency must be added here
consciously, never inherited silently. Records captured this way carry the
stdlib logger name in `caller` (e.g. `httpx._client.send`) rather than one
of our own `module.function` values — expected, and worth knowing when
querying that field.

### D2. One facade, four methods, explicit destinations

The two output channels remain physically distinct — a redraw-driven
terminal stream and a JSON file — but there is now exactly one API in
front of both. Destination is a property of the *method*, not a flag:

| Method | Terminal | JSON record | Gated |
|---|---|---|---|
| `.interactive(msg, **f)` | yes (via active bar, D3) | yes, `level: "interactive"` | never |
| `.error(msg, exc_info=…, **f)` | yes (stderr) | yes, `level: "error"` | never |
| `.info(msg, **f)` | **no** | yes, `level: "info"` | never |
| `.debug(msg, **f)` | no | yes, `level: "debug"` | yes — `debug` only |

`.interactive()` is the human channel: everything the operator sees on
screen, and therefore everything reconstructable from the file afterwards.
It replaces every pipeline `print()` one-for-one, so terminal output keeps
its exact current shape.

`.info()` is deliberately **file-only**. This is a refinement of the
original two-level sketch, and it is what makes the facade worth having:
there are many events worth recording (a stage entered, a dispatch queued,
a digest matched) that must not become terminal noise. Without this split
you can only choose between "on screen and in the file" and "nowhere", and
the interactive output degrades as instrumentation grows. `LOG_LEVEL`
still gates volume; it no longer decides *destination*.

Multi-line presentation blocks — the final report from
`modules/ingest_report.py:format_report()` — are one `.interactive()` call
producing **one** record with the block intact in `message`, not thirty
records. The file mirrors what the operator saw, at the same granularity.

### D3. Progress bars are constructed by the facade

`Progress` and `WorkerPoolProgress` stay in `modules/progress.py` — 336
lines of careful redraw, sliding-window rate, ETA, and heartbeat logic with
no reason to move. What changes is ownership of the *interface*: the nine
call sites (`modules/pipeline/discover.py:71`, `emails.py:166`,
`pdfs.py:110,327,389`, `summaries.py:108`, `embed.py:152`,
`modules/accuracy.py:280,412`) stop importing `modules/progress.py` and
call the facade instead:

```python
progress = log.progress("parse emails", total=len(candidates))
pool = log.worker_pool("pdf to text", workers=10, total=33)
```

The factory returns the same widget objects, pre-registered with the
facade. Two things follow automatically:

- **No redraw corruption.** A bare `print()` during a carriage-return
  redraw shreds the line — `Progress.println()`
  (`modules/progress.py:89-96`) exists for exactly this. Because the facade
  knows which bar is live, `.interactive()` and `.error()` route through
  `println()` while one is active and to `print()`/stderr otherwise. Call
  sites never think about it.
- **Lifecycle records, not redraw records.** A bar emits one `.info()`
  record on `done()` carrying label, total, elapsed, and rate. The
  thousands of intermediate redraws produce nothing — they are UI frames,
  not events. `progress.println(...)` (used for per-item warnings at
  `modules/accuracy.py:293,421,432,449`) becomes `.interactive()`, so those
  *are* recorded.

`modules/progress.py`'s only edit is register/deregister on
first-output/`done()`; drawing behaviour is untouched.

### D4. Level resolution

Level resolves once at process start, highest precedence first:
`LOG_LEVEL` environment variable → `logging.level` in `config.yaml` →
`"info"`. An unrecognized value warns to stderr and falls back to `info`
rather than failing a long ingest over a typo.

- `LOG_LEVEL=info` (default) — `.interactive()`, `.error()`, `.info()`
  records are written. `.debug()` is a true no-op: no record, no
  formatting, no file write, no kwarg evaluation beyond the level check.
- `LOG_LEVEL=debug` — all four are written, and `httpx`/`httpcore` DEBUG is
  attached. **Terminal output is identical to `info`** — raising the level
  never adds screen noise, only file detail.

The env var is the codebase's first (`modules/` currently reads zero
environment variables) and deliberately sits *outside* the frozen `Config`
dataclass: logging must be configurable before and independently of config
load, including for failures during config load itself. `logging.level` in
`config.yaml` is added to `_YAML_KEYS` for a durable default; the env var
overrides it per-invocation.

### D5. Record schema — six fields, no duplicates

| Field | Type | Example | Notes |
|---|---|---|---|
| `timestamp` | string | `"2026-07-26T10:25:48.123Z"` | RFC 3339 / ISO 8601, UTC, milliseconds. |
| `run_id` | string (UUID4) | `"3fae1b2c-…"` | One per CLI invocation; stitches every record from that execution. |
| `worker_thread` | string | `"pdf-transform_3"` | `threading.current_thread().name`. |
| `caller` | string | `"pipeline.summaries.run"` | `module.function_name`, derived from the record's own frame metadata — call sites never pass it, so it cannot drift on a rename. |
| `level` | string | `"error"` | `"interactive"` \| `"error"` \| `"info"` \| `"debug"`. |
| `message` | string | `"inference endpoint unreachable"` | Free text; carries whole multi-line blocks for `.interactive()`. |

`**fields` kwargs (`thread_id=10`, `endpoint="http://…"`) merge into the
same flat object — that is what makes the log queryable rather than merely
greppable. A kwarg colliding with a schema field name is rejected loudly at
call time. `.error(..., exc_info=exc)` adds `exception_type`,
`exception_message`, and `traceback`.

One JSON object per line, no pretty-printing.

**D5a. Why RFC 3339 with milliseconds, as the single time field.**
It is the one format that is simultaneously human-readable, lexically
sortable, unambiguous about timezone, and natively parsed by every
mainstream observability backend — OpenObserve, Elasticsearch, Loki,
Splunk, Datadog, Vector, Fluent Bit. Millisecond precision preserves
ordering across the ten concurrent `pdf-transform` workers, which the
originally specified `"YYYYMMDD HH:MM:SS"` could not (no timezone, and
second resolution collapses concurrent records into ties). One field, no
duplication.

*OpenObserve note:* it auto-detects a field literally named `_timestamp`
and otherwise falls back to ingestion time. Since the stream's time field
is a one-time per-stream setting, point it at `timestamp` at stream
creation rather than carrying an underscore-prefixed, vendor-specific name
in every record.

**D5b. Why the thread name, as the single thread field.**
`threading.get_ident()` returns an opaque integer that is **reused after a
thread dies** — so grouping a long run by it can silently merge two
different workers, which is precisely the ambiguity to avoid. All three
pools already set `thread_name_prefix` (`pdf-transform` at
`modules/pipeline/pdfs.py:420`, `summary-gen` at `summary_dispatch.py:75`,
`embed-dispatch` at `modules/embedding/dispatch.py:180`), so
`ThreadPoolExecutor` names threads `<prefix>_<n>`, the main thread is
`MainThread`, and names are unique within one `run_id` given one pool per
prefix per run — true today for all three pools. If a future stage ever
recreates a pool with a repeated prefix inside one run, revisit this.

Unrelated to the pdfs stage's caller-assigned `worker_id` slot index
(`pdfs.py:392`), which is a job-pool concept and stays as-is.

### D6. Reachability: process-scoped facade, `ctx.log` as an alias

Injection through `PipelineContext` alone cannot reach the code that most
needs to log: `modules/inference.py:59` takes `config`, not `ctx` — and
that is where the motivating failure originated. The same is true of
`modules/embedding/backends.py` and much of `modules/retrieval.py`.
Threading a `log` parameter through every constructor is a large refactor
with no payoff.

`modules/logs.py` exposes `get_log()`, returning the current execution's
facade. It is configured exactly once, by `setup_logging()` at the CLI
entrypoint, before any stage runs; no reconfiguration, no mutation after
that point. This is write-once process initialization, not the
module-global mutable state that `modules/config.py`'s docstring rules out.
`PipelineContext.log` is a convenience alias to the same object.

Before `setup_logging()` runs (and in tests that never call it),
`get_log()` returns a null facade that discards records and writes terminal
output straight to stdout/stderr — importing a module must never require
logging to be configured, and a test that prints must not crash.

Writes go through `QueueHandler` → `QueueListener` → a single writer
thread, so ten concurrent workers never contend on the file handle and a
slow disk never stalls a pipeline stage.

### D7. Correlating the terminal session with the file

The `run_id` is printed **once at the start and once at the end** of a run,
not on every line:

```
pocket-advisor: run 3fae1b2c-9d4e-4c1a-8b2f-7a1e6d0c9b34 — workspace test
…
Run report: …/ingest-runs/20260725T233950942056Z.json
Run log:    …/execution-logs/20260726-102548.jsonl (run 3fae1b2c-…)
```

Prefixing 36 characters of UUID onto every interactive line would wreck the
readable output this design exists to preserve, and would break progress-bar
width on a TTY. A banner plus footer gives the operator the exact token to
paste into an OpenObserve query, with zero ongoing noise; every line of the
session is already correlated inside the file by the `run_id` field.

The end-of-run footer prints even on failure and on `KeyboardInterrupt` —
the runs worth correlating are the ones that broke.

### D8. Location: workspace state, preserved across wipes

`workspaces/.state/workspace-<id>/execution-logs/<YYYYMMDD-HHMMSS>.jsonl`,
one file per execution (process start time; a `-1` suffix loop guards the
theoretical same-second collision, mirroring
`modules/ingest_report.py:512-520`).

Not a repo-root `logs/`. These records contain case data by construction —
the motivating transcript alone logs an account number
(`732-250 742 481`), statement filenames, and email subjects. Root
`.gitignore:1-4` blankets `workspaces/` precisely because "ALL user/case
data... Platform code stays case-free," and `AGENTS.md` hard rule 1 makes
`workspaces/.state/` the home for regenerable derived state. Living inside
that tree inherits both the ignore rule and workspace scoping
automatically, instead of making one new `.gitignore` line the only thing
between an account number and a commit. No `.gitignore` change is needed.

Execution logs must nonetheless **survive `wipe state`** — post-mortem
history is worthless if the recovery step erases it. `modules/wipe.py:12`
already has the mechanism: add `"execution-logs"` to
`PRESERVED_STATE_NAMES`. `wipe state` then lists it alongside
`search-accuracy-tests`; that section's "Preserved workspace test data"
heading is retitled to "Preserved" since it now covers two distinct kinds.

Note the name collision with the existing `Config.logs_dir`
(`workspace-<id>/logs/`, holding `review_queue.csv`, the transaction
manifest, and `ingest-runs/`). Disambiguate as **state logs** (existing,
wiped with state) versus **execution logs** (new, preserved). A distinct
sibling directory, rather than a subdirectory of `logs/`, is what makes the
one-line preserve-rule possible.

### D9. Correlate with the existing run report

`modules/ingest_report.py` already persists a per-run JSON record. Two
independent identities for one run means no pivot from a report to its
logs. `run_id` is added to `IngestRunReport`, `REPORT_SCHEMA_VERSION` bumps
4 → 5, and `build_report()` takes it alongside `started_at`.

### D10. Durability on crash and interrupt

The runs worth reading are the ones that died. `modules/cli.py:825` already
catches `KeyboardInterrupt`, and a hung endpoint is the motivating failure
mode, so a buffered tail lost at exit would defeat the feature.

The writer flushes after every `interactive`/`error`/`info` record. `debug`
records are buffered for throughput and flushed on a 1s interval or on any
higher-level record. `setup_logging()` returns a context manager whose
`finally` stops the `QueueListener`, drains the queue, and prints the D7
footer; the CLI entrypoint wraps the entire dispatch — including the
`KeyboardInterrupt` handler — in it.

## Non-goals

- **The query daemon.** `modules/daemon.py:222 serve()` runs up to 1800s
  across many queries; "one file per execution" would give one unbounded
  file with no per-query correlation. Instrumenting it needs a
  session-plus-request id design of its own, deferred to a later round.
- Rewriting `Progress`/`WorkerPoolProgress` drawing behaviour, or changing
  the shape of any interactive output. D3 changes construction and
  registration only.
- Per-redraw JSON records. Progress bars emit lifecycle records only.
- Migrating the direct operator-invoked command prints in `modules/cli.py`
  (query, accuracy, index-list, verification).
- An OpenObserve shipper, agent, or dashboard. This design produces
  ingestible files; wiring them in is a separate step once the format is
  validated against real runs.
- Log rotation or automated retention. Files are small and per-run; the
  operator prunes `execution-logs/`.
- A redaction/scrubbing layer. Case data in execution logs is accepted,
  contained by D8's location.
- A `warning` level. The WARNING tier the ingest report already models
  (`pdf_ocr_warnings`, `pdf_weak_dates`) is expressed as
  `.interactive(msg, severity="warning")` — queryable as a field without
  adding a level.

## Target shape

```python
# modules/logs.py
def setup_logging(config: Config, *, run_id: str) -> AbstractContextManager
def get_log() -> Log

class Log:
    # records
    def interactive(self, message: str, **fields: Any) -> None: ...
    def error(self, message: str, *, exc_info: BaseException | None = None,
              **fields: Any) -> None: ...
    def info(self, message: str, **fields: Any) -> None: ...
    def debug(self, message: str, **fields: Any) -> None: ...
    # progress widgets (registered, lifecycle-recording)
    def progress(self, label: str, total: int | None = None) -> Progress: ...
    def worker_pool(self, label: str, workers: int,
                    total: int) -> WorkerPoolProgress: ...
```

```json
{"timestamp": "2026-07-26T10:25:48.123Z", "run_id": "3fae1b2c-9d4e-4c1a-8b2f-7a1e6d0c9b34", "worker_thread": "summary-gen_1", "caller": "pipeline.summary_dispatch._report", "level": "error", "message": "summary generation: inference endpoint unreachable", "endpoint": "http://127.0.0.1:8000/v1/chat/completions", "exception_type": "RemoteProtocolError", "exception_message": "peer closed connection without sending complete message body (incomplete chunked read)", "traceback": "Traceback (most recent call last):\n  …"}
```

## Call-site migration (this round)

**Prints → `.interactive()` / `.error()`:**

| Site | Method |
|---|---|
| `modules/pipeline/base.py:72` (stage summary) | `.interactive()`, `stats` as fields |
| `modules/pipeline/summaries.py:99,119` | `.interactive()` / `.error()` |
| `modules/pipeline/summary_dispatch.py:109` | `.error(exc_info=…)` on failure, else `.interactive()` |
| `modules/embedding/dispatch.py:77-84,254` | `.interactive()` / `.error()` |
| `modules/pipeline/embed.py:127,169,191` | `.interactive()` / `.error()` |
| `modules/pipeline/pdfs.py:103` | `.interactive(severity="warning")` |
| `modules/inference.py:172-176` (`_probe`) | `.error(exc_info=exc)` — the motivating gap |
| `modules/cli.py:198` (final report block) | one `.interactive()` |

**Progress construction → facade factories** (drop the
`from modules.progress import …` line in each): `discover.py:71`,
`emails.py:166`, `pdfs.py:110,327,389`, `summaries.py:108`,
`embed.py:152`, `accuracy.py:280,412`. Their `println()` calls
(`accuracy.py:293,421,432,449`) become `.interactive()`.

## Implementation order

Each step leaves the suite green.

1. **`modules/logs.py`** — `Log` with the four record methods, JSON
   formatter, `QueueHandler`/`QueueListener`, level resolution,
   `setup_logging()`, `get_log()`, null facade, `exc_info` handling.
   `modules/tests/test_logs.py`: schema shape and field types; destination
   matrix (`.info()` writes a record and **nothing** to the terminal;
   `.interactive()` does both); `.debug()` under `LOG_LEVEL=info` produces
   **zero file writes** (assert on the file, not on display); `run_id`
   identical across every record; 10 concurrent threads produce 10×N
   valid, non-interleaved lines; kwarg collision raises; `exc_info`
   captures type/message/traceback; `get_log()` before `setup_logging()`
   is silent and does not crash.
2. **Progress ownership** (D3) — register/deregister in
   `modules/progress.py`; `log.progress()`/`log.worker_pool()` factories;
   lifecycle record on `done()`. Test that an `.interactive()` call during
   an active bar routes through `println()` and leaves no redraw residue,
   and that a bar emits exactly one record per run, not per step.
3. **Wire the entrypoint** — `run_id` generation, `setup_logging()`
   wrapping dispatch and the `KeyboardInterrupt` handler, D7 banner and
   footer in `modules/cli.py`; `PipelineContext.log` alias in
   `modules/pipeline/base.py`.
4. **Preserve across wipes** — `"execution-logs"` into
   `PRESERVED_STATE_NAMES` (`modules/wipe.py:12`), retitle the preserved
   heading, extend the wipe test to assert execution logs survive.
5. **Report correlation** (D9) — `run_id` on `IngestRunReport`,
   `REPORT_SCHEMA_VERSION` 4 → 5, update
   `modules/tests/test_ingest_report.py`.
6. **Migrate the call sites** in both tables above.
7. **Third-party capture** — attach `httpx`/`httpcore` at DEBUG under
   `LOG_LEVEL=debug` only; assert nothing is attached at `info`.
8. Full verification.

## Verification

```bash
for test_file in modules/tests/test_*.py; do
  uv run python "$test_file"
done
uv run pocket-advisor.py test
git diff --check
```

Then, on the test workspace, `ingest all` under both `LOG_LEVEL=info` and
`LOG_LEVEL=debug`, confirming:

1. One `.jsonl` per run under
   `workspaces/.state/workspace-test/execution-logs/`.
2. Every line parses as JSON and carries all six schema fields.
3. `run_id` is identical across every line of one file, differs between
   runs, and matches the banner, the footer, and the persisted ingest
   report.
4. `.debug()` and `httpx`/`httpcore` records appear only under
   `LOG_LEVEL=debug`; terminal output is byte-identical between the two
   levels.
5. Interactive stdout is byte-identical to before the change apart from
   the D7 banner and footer (diff a captured run), and progress bars show
   no redraw corruption on a TTY.
6. Replaying only the `level: "interactive"` records of a run reproduces
   that run's terminal transcript.
7. `wipe state` preserves `execution-logs/` and says so.
8. Ctrl+C mid-stage still leaves a complete, valid `.jsonl` whose last
   records describe the interruption, and still prints the footer.
