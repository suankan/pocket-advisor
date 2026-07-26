# Inference Dispatch Queues and Live Observability

Status: **shipped 2026-07-26**, design `64ccb4b`, implemented across
`491e012`, `2f80477`, `ff702b7`, `e3b7e70`, `e202a52`, `0f3a1d5`.
Supersedes the 2026-07-26 draft of this file, whose premise ("decouple
embedding behind a queue so it starts before all documents are ready")
describes behavior that already shipped — see "What already exists" below.
The residual, real gap was observability, not throughput.

Implementation map:

- `modules/dispatch.py` — `BoundedInferenceDispatcher`, `QueueSnapshot`,
  the live-dispatcher registry and `cancel_all()`.
- `modules/progress.py` — `LiveDisplay`, `QueuePanel`, and the panel
  protocol both progress widgets now render through.
- `modules/embedding/dispatch.py` — `EmbedDispatcher.retarget()`.
- `modules/pipeline/embed.py` — barrier drain, dispatcher reuse, close.
- Tests: `modules/tests/test_dispatch.py`,
  `modules/tests/test_progress_display.py`,
  `modules/tests/test_embed_dispatcher_reuse.py`.

## Purpose

Embedding work is dispatched asynchronously from three producer stages and
drains across the whole run, but nothing renders its state while it is in
flight. An operator watching `ingest all` sees the current stage's progress
bar and no indication of how much embedding work is outstanding, how fast it
is draining, or whether it is stalled.

This design makes inference dispatch pressure continuously visible on stdout:
the queue growing as producers submit, and draining as workers complete. It
unifies the two dispatchers behind one base class so that reporting has a
single source, and collapses embedding to a single dispatcher instance whose
lifetime spans the run.

No throughput change is intended or claimed.

## What already exists (do not re-propose)

Verified against the implementation on 2026-07-26:

- **Producers dispatch at readiness.** `docs/inference/inference-serving.md`
  decision 5 is implemented. `modules/pipeline/pdfs.py` dispatches **per
  document** as each worker completion is published, while slower PDF
  transforms continue;
  `modules/pipeline/summaries.py` dispatches per generated summary;
  `modules/pipeline/emails.py` dispatches at end of stage. The dispatcher is
  run-wide (`PipelineContext.embed_dispatcher`), so vectors publish
  concurrently with the pdfs, thread, and summaries stages. The `embed` stage
  is a convergence and matrix-rebuild pass, not the point where embedding
  begins.
- **Leaf and summary embedding are already one queue.** `EmbedDispatcher` has
  one `ThreadPoolExecutor` and one `_futures` list; every submission goes
  through the same `_submit()`. The `leaf`/`summary` split is a telemetry
  label selecting a counter bucket (`EmbedQueues`, `modules/telemetry.py`),
  not a second queue. The claim in
  `docs/ingestion/chunking-and-embedding.md` that "pending passages run in
  independent leaf/summary queues" is inaccurate and is corrected by this work.
- **The counters largely exist.** `EmbedQueueTelemetry` tracks
  `dispatched_at_readiness`, `successful_entities`, and `failed_entities`,
  updated inside the worker task under the dispatcher lock.

Two ideas from the draft are rejected outright:

- **Discovery as a producer for embedding.** Discovery enumerates and hashes
  candidate files; no text exists at that point. The producers of embeddable
  text are necessarily the stages that publish text artifacts (emails 2b,
  pdfs). Discovery could only produce *for those stages*, which is a
  cross-stage streaming redesign already deferred by
  `docs/ingestion/pdf-to-text-pipeline-design.md` ("cross-stage OCR/GPU
  overlap and multiple SQLite writers") against the single-coordinator SQLite
  invariant.
- **A queue as a speedup.** Per `docs/ingestion/ingestion-performance.md` the
  measured split is summaries 60%, embed 21%, pdfs 19%, everything else about
  three seconds. Email-body embedding already overlaps the remaining 79% of
  the pipeline. There are no seconds to recover here.

## Locked decisions

1. **One embedding dispatcher instance per run.** Today a second
   `EmbedDispatcher` is constructed inside `EmbedStage._converge_pending`
   after `_settle_readiness_dispatch()` drains, closes, and discards the
   shared one. The two are never concurrent — it is a sequential handoff. The
   convergence sweep's real requirement is a **settled vector cache** before
   it globs `vecs_dir` to decide what is pending; that is a barrier, which
   `drain()` already provides while leaving the instance reusable (it swaps
   `_futures` for a fresh list and keeps the pool alive; only `close()` shuts
   the pool down).

   Collapsing to one instance is what makes whole-run reporting possible:
   cumulative totals survive the readiness→convergence transition instead of
   resetting to zero when the embed stage starts. `at_readiness` becomes a
   submission attribute rather than an implicit "which instance am I".

   Consequence: the backend and fingerprint currently passed to the
   convergence instance's constructor must be settable on the shared
   instance, and `check_ready()` needs a call site that is not construction.

2. **Summary generation stays a separate queue.**
   `EmailThreadsSummaryDispatcher` is a different workload (chat completions)
   against a different endpoint (`summarisation_endpoint`), and it *does* run
   concurrently with embedding dispatch. Eight in-flight against the
   embedding endpoint and eight against the summarisation endpoint are
   independent capacity budgets. Merging the work into one queue would let
   slow generations starve embeddings and is rejected.

   They share a base class, not a queue.

3. **`BoundedInferenceDispatcher` is the shared base.** The two dispatchers
   are already structurally near-identical: same bounded
   `ThreadPoolExecutor(INFERENCE_MAX_IN_FLIGHT)`, same `_futures` list, same
   `pending_count`, same `submit → bool`, same `_task → outcome` carrying
   `error`/`skipped`, same `drain(progress) → (done, failed, skipped,
   outcomes)`, same `abandon`/`close`/`unavailable`/`_LIVE` registration. Two
   artifacts confirm the duplication is unintentional:
   `modules/pipeline/summary_dispatch.py` imports the private `_LIVE` across a
   module boundary, and carries
   `self._lock = None  # unused placeholder for symmetry; not needed`.

   The base owns the pool, the lifecycle, the live counters, and the one
   reporting hook. Subclasses keep their genuine difference: `EmbedDispatcher`
   publishes its vector inside the worker; `EmailThreadsSummaryDispatcher`
   settles on the main thread after `drain()`.

   The base lives in its own module so neither concern imports the other's
   privates.

4. **Completion is counted in the worker, not at drain.** Today outcomes are
   only observed when `drain()` walks futures in submission order — which for
   readiness dispatch happens at the embed stage, long after the work. Live
   counters are incremented inside `_task`, under the lock that already
   guards telemetry mutation there.

   The base maintains `submitted`, `started`, `finished`, `failed`, and
   `skipped`, and exposes an immutable snapshot:

   ```text
   queued    = submitted - started
   in_flight = started - finished - failed - skipped
   done      = finished
   ```

5. **One terminal region owner.** Both `Progress` (single-line carriage-return
   redraw) and `WorkerPoolProgress` (N-line ANSI cursor-up block) assume they
   exclusively own the bottom of stderr. That holds today only because bars
   are strictly sequential — each is created and `done()` before the next.
   `Progress.println()` exists precisely to funnel log lines through the one
   active bar, encoding the single-owner assumption.

   A queue display spanning the run is by definition concurrent with stage
   bars. Two bars redrawing from different threads corrupt each other. So a
   `LiveDisplay` becomes the sole owner of the stderr bottom region, and
   existing bars render *through* it rather than writing directly.

6. **No configuration knob.** The display is always on and degrades
   automatically on a non-TTY. Nothing here is operator-tunable.

7. **No producer backpressure is introduced.** `dispatch.py`'s module
   docstring currently claims "a saturated pool gives the submitting producer
   backpressure". That is false: `max_workers` bounds concurrent *execution*,
   while `ThreadPoolExecutor`'s internal queue is unbounded, so
   `submit_pending_leaves` queues every pending payload without ever blocking
   its producer.

   The claim is deleted rather than implemented. Producers must "submit and
   move on, never blocking the pipeline" (`modules/pipeline/base.py`); real
   backpressure would block a producer stage and would also *hide* the queue
   pressure this design exists to display. Resident payload memory (about
   9.6k chunks on the reference workspace) is an accepted cost, recorded
   under "Explicitly deferred".

## Display design

### LiveDisplay

One owner per stream (`display_for`) of the terminal's bottom region,
holding an ordered list of registered panels. A panel supplies
`lines() -> list[str]`; it never writes to the stream itself.

**Locking rule, load-bearing:** the display lock is innermost. A widget may
call into the display while holding its own lock; the display must never
call back into a widget's lock. `lines()` is therefore cached and takes no
lock, while `refresh()` takes the widget lock and is only ever invoked from
outside the display lock. Inverting either side deadlocks the heartbeat
against `step()`, which `test_progress_display` hammers concurrently.

- `register(panel)` / `unregister(panel)` — under the display lock.
- `redraw()` — cursor up over the previously drawn line count, clear, write
  every panel's current lines, record the new count. Lifted from
  `WorkerPoolProgress`'s block technique and extended: when the block
  shrinks (a stage bar left), the vacated lines are wiped so no stale row
  survives below the new block.
- `finalize(panel, lines)` — scroll a finished widget's last line
  permanently above the live region and drop it, so pinned panels keep
  drawing below it.
- `println(msg)` — clear the block, write the real log line, redraw. This
  is the single implementation behind both widgets' `println`.
- One heartbeat thread for the whole display, replacing the per-widget
  heartbeat threads.

Panel order is stable: transient stage panels first, persistent queue panels
last, so the queue rows hold a fixed position while stage bars come and go
above them.

`Progress` and `WorkerPoolProgress` keep their public API — `step`, `start`,
`begin`, `finish`, `println`, `done` — and their rate/ETA windowing. Only
their emit path changes: they register a panel on construction, format lines
on demand, and unregister in `done()`. The observer/`detach` lifecycle
summary is untouched.

### Non-TTY

No redraw and no cursor control. Each panel emits a plain line on its own
`quiet_every` cadence, exactly as today, and the queue panel joins that
rotation. Piped logs stay readable.

### Queue panel

One line per live dispatcher, rendered from the snapshot in decision 4:

```text
collect pdfs: 34/458 (7%)  2.1/s  eta 3m22s [16s] — invoice_2019.pdf
  embed queue:   1240 queued · 8 in flight · 3200 done · 12 failed  41.3/s
  summary queue:   18 queued · 8 in flight ·  104 done             0.4/s
```

A dispatcher registers its panel on first submission and unregisters at
`close()`, so a run that embeds nothing shows no row. Rate uses the same
sliding ~30s window as `Progress`, not a lifetime average.

Deliberately absent: a percentage and an ETA. The denominator grows while
producers are still submitting, so both would be actively misleading for most
of a run. Queue depth is the honest pressure signal. As shipped, neither is
shown at any point — a completion percentage once producers finish remains
possible but was not adopted, since the panel cannot know that producers are
done.

Failed and skipped counts appear only when non-zero. Off a TTY the row
degrades to a plain line on the existing quiet cadence plus one summary line
at close, so a piped log still records what the queue did.

## Ancillary corrections

Small, contained, and done as part of this work:

- Delete the false backpressure sentence from `modules/embedding/dispatch.py`
  (decision 7).
- Correct "independent leaf/summary queues" in
  `docs/ingestion/chunking-and-embedding.md` to one queue with two counter
  buckets.
- Rename `EmbedQueueTelemetry.pending_entities` to `processed_entities`. The
  field is incremented on *completion*, so the name inverted its meaning; the
  validation invariant (`successful + failed == processed_entities`,
  `modules/telemetry.py`) reads correctly only under the new name. This
  touched `_QUEUE_COUNTS`, the saved-report loader and renderer, and
  `docs/ingestion/ingest-all-reporting.md`. Saved-report schema change is
  permitted where it improves observability
  (`docs/ingestion/pdf-to-text-pipeline-design.md`). Live queue depth is not
  stored in telemetry at all — it is read from the dispatcher's
  `snapshot()`.

## Acceptance criteria

1. During `ingest all` on a cold workspace, the embedding queue row is
   visible and updating while the pdfs stage's own progress is also live, and
   neither display corrupts the other.
2. The queue row shows depth rising as producers submit and falling as
   workers complete, with completions reflected within one heartbeat of the
   worker finishing — not deferred to `drain()`.
3. Embedding uses exactly one `EmbedDispatcher` instance for the whole run.
   Its cumulative counters do not reset when the embed stage begins its
   convergence pass.
4. The convergence sweep still runs against a settled vector cache: no entity
   is dispatched twice because a readiness publication was still in flight
   when the sweep globbed `vecs_dir`.
5. Both dispatchers derive from `BoundedInferenceDispatcher`; neither module
   imports a private name from the other; the "unused placeholder for
   symmetry" lock is gone.
6. Summary generation and embedding retain independent in-flight budgets of
   `INFERENCE_MAX_IN_FLIGHT` each.
7. Non-TTY output contains no ANSI cursor control and emits queue lines on the
   existing quiet cadence.
8. An interrupt still abandons queued work through `cancel_all()`, the
   display releases the terminal cleanly, and abandoned entities remain
   durable pending gaps for the next `ingest embed`.
9. A run that dispatches nothing draws no queue row and leaves no empty cache
   directories.
10. Vector identity, fingerprints, publication discipline, and per-entity
    failure isolation are unchanged. All tests use temporary synthetic
    fixtures; the existing suites pass.

## Verification performed

- All 19 self-test suites pass, including three new ones.
- Criterion 4 was checked by mutation: deleting the barrier drain makes
  `test_embed_dispatcher_reuse` fail. It fails on `retarget()`'s idle guard,
  which is the stronger outcome — the race becomes a loud error rather than
  a silent double-embed.
- The concurrent-render arrangement (criterion 1) was driven through a
  pseudo-terminal and replayed in-process through an ANSI interpreter: a
  stage bar and the pinned queue row composite correctly, `println` output
  scrolls above the live region, the stage summary is retained, and the
  queue row is removed cleanly at close.
- **Not verified end-to-end:** a real `ingest all` against a running oMLX
  instance over a live workspace. Every check above uses synthetic fixtures
  or fake terminals. The first real run is where display behavior under a
  scrolling 24-row terminal and true multi-minute stage timing gets
  exercised.

## Explicitly deferred

- **Bounded submission and lazy payload derivation.** Resident payload memory
  is accepted (decision 7). Deriving chunk payloads inside the worker instead
  of eagerly in `submit_pending_leaves` would reduce it, but `ChunkReader`
  holds the SQLite connection and its thread-safety would have to be
  established first.
- **Incremental email-body dispatch.** The emails stage dispatches once, after
  the corpus-wide `compact_authored_bodies` pass, because an email's authored
  body depends on its `In-Reply-To` parent being registered and import-order
  independence is a locked acceptance criterion. An order-independent partial
  rule exists (dispatch immediately when an email has no `In-Reply-To`, or its
  parent is already registered; defer the rest), but the measured profile puts
  the win in seconds.
- **Cross-stage streaming** (discovery feeding parse feeding embed as one
  continuous pipeline) and multiple SQLite writers.
- **A general multi-region terminal UI.** `LiveDisplay` owns one bottom
  region with a flat panel list; nested or scrolling regions are out of scope.
