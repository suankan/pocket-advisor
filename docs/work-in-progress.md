# Work in Progress

Scratch pad for the feature currently being implemented. When idle, this file
is intentionally near-empty.

When a roadmap item is picked up, move it here with any active context needed
to resume work. When the item is done, its final state is locked down in the
feature design doc, added to `docs/changelog.md`, and removed from this file.

See the lifecycle workflow in `AGENTS.md`.

## Current work items

### Inference dispatch queues and live observability

Roadmap item 1. Design locked in
`docs/ingestion/embedding-queue-and-workers.md` (proposed 2026-07-26).
Observability only — no throughput change is intended or claimed.

Implementation order (each step leaves the suites green):

1. **`BoundedInferenceDispatcher` base**, in its own module so neither
   concern imports the other's privates (`summary_dispatch.py` currently
   imports the private `_LIVE` from `modules/embedding/dispatch.py`). Owns
   the pool, `_futures`, `unavailable`, `_LIVE` registration,
   `drain`/`abandon`/`close`. Pure refactor, no behavior change.
   Drop the `self._lock = None  # unused placeholder for symmetry` line.
2. **Worker-side live counters** on the base: `submitted`, `started`,
   `finished`, `failed`, `skipped`, incremented inside `_task` under the
   lock that already guards telemetry there, plus an immutable snapshot
   (`queued = submitted - started`,
   `in_flight = started - finished - failed - skipped`).
3. **One `EmbedDispatcher` per run** — decision 1. `_settle_readiness_dispatch`
   becomes a barrier `drain()` without `close()`; `_converge_pending` reuses
   `ctx.embed_dispatcher`. Needs backend/fingerprint settable post-construction
   and a `check_ready()` call site that is not the constructor.
   **Watch acceptance criterion 4**: the convergence sweep decides pending
   work by globbing `vecs_dir`, so the barrier must be provably complete
   before the glob or entities dispatch twice. Fixture required.
4. **`LiveDisplay`** owning the stderr bottom region; `Progress` and
   `WorkerPoolProgress` register panels and render through it instead of
   writing directly. Lift `WorkerPoolProgress._clear_block`'s ANSI technique
   into the display; consolidate the per-bar heartbeat threads into one.
   Public bar APIs and the observer/`detach` lifecycle are unchanged.
5. **Queue panel** — one row per live dispatcher, registered on first
   submission, unregistered at `close()`. No percentage or ETA while
   producers are still submitting.
6. **Ancillary corrections** — delete the false backpressure sentence in
   `modules/embedding/dispatch.py`; correct "independent leaf/summary
   queues" in `docs/ingestion/chunking-and-embedding.md`; rename
   `EmbedQueueTelemetry.pending_entities` to `processed_entities`
   (touches `_QUEUE_COUNTS`, the saved-report loader, and
   `docs/ingestion/ingest-all-reporting.md`). Step 6 is separable if the
   report-schema churn is unwanted.

Open context to carry: no config knob and no producer backpressure are to
be added — both are locked decisions (6 and 7), and backpressure would
hide the very pressure this work displays.
