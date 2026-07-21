# Pocket Advisor — Thread-Summary Generation Concurrency

Status: proposed 2026-07-20; supersedes nothing. Companion to
`docs/features/embedding-design-v2.md` (which moves all inference to the
external oMLX server). This design parallelizes the summarization *generation*
stage only; embedding of generated summaries is already concurrent through
`EmbedDispatcher` and is unchanged.

## Problem

`ingest all` runs the summaries stage serially: `modules/pipeline/summaries.py`
iterates `for job in stale:` and, for each thread, blocks on a single
`generator.generate()` HTTP call to oMLX's `/v1/chat/completions` before
writing the database row and moving to the next thread. With 126 stale threads
on a real workspace this crawls at ~0.1 threads/s with a multi-minute ETA,
because only one long-context 4B decode is ever in flight even though oMLX's
continuous batching (`max_concurrent_requests=8`) could serve many.

Embeddings (email bodies, PDF text, and the summaries' own vectors) are
*already* concurrent: producers dispatch at artifact readiness via
`EmbedDispatcher`, a `ThreadPoolExecutor(max_workers=INFERENCE_MAX_IN_FLIGHT)`
(`modules/embedding/dispatch.py`). Only generation is serial.

## Core idea

Add a purpose-built `EmailThreadsSummaryDispatcher` that fans the generation
loop out across a bounded pool of threads, exactly as `EmbedDispatcher` does
for embeddings — but owning only generation concerns. The two dispatchers
share the *pattern* (bounded pool, weakref registry, interrupt abandon,
unavailable degradation) and the *width constant* (`INFERENCE_MAX_IN_FLIGHT`),
not a common base class. We deliberately do **not** generalize
`EmbedDispatcher` into a generic `InferenceDispatcher`: its `_task` is coupled
to per-entity numpy file publication and embed-specific telemetry, whereas
generation's "publication" is a database upsert that must stay serialized on
the main thread. Separating the concerns keeps each dispatcher honest about
what it owns.

## Locked decisions

1. **Separate dispatcher, not a generic base.** A new
   `EmailThreadsSummaryDispatcher` lives beside the summaries stage
   (`modules/pipeline/summary_dispatch.py`). `EmbedDispatcher` is untouched.
   No `InferenceDispatcher` superclass is introduced.

2. **One pool task per thread.** The entire `self._generate(job, generator,
   …)` — including any internal hierarchical segment/reduce sequencing — runs
   as one task on a worker thread. Intra-thread segment calls are *not* fanned
   out this pass; the reduce tree is inherently sequential per level and stays
   inside the worker. This is the smallest change that removes the serial
   bottleneck (1 → up to `INFERENCE_MAX_IN_FLIGHT` threads in flight).

3. **Default width = `INFERENCE_MAX_IN_FLIGHT` (8).** Generation uses the
   same constant as embeddings, per explicit instruction. Splitting a lower
   `summary_max_in_flight` (e.g. 4, appropriate for 4B decodes) is a deferred
   follow-up, not part of this design.

4. **Only inference runs off-thread.** Workers call `generator.generate()`
   and mutate their own per-task `metrics`/`timings` objects. They never touch
   the database connection, the `Progress` bar, or the `ReviewLog`. All of
   those run on the main thread after `drain()` resolves each future. This
   preserves sqlite single-writer safety and `Progress` (a shared,
   non-thread-safe bar) integrity.

5. **Database write + summary-embed dispatch stay on the main thread.** After
   `drain()`, the stage iterates outcomes in submission order and, for each
   success, performs the existing `INSERT … ON CONFLICT(thread_id) DO UPDATE`
   + `conn.commit()` and the `embed_dispatcher.submit_summary(...,
   at_readiness=True)` call. Failure handling (rollback, `review.flag`,
   commit, `stats.inc("failed")`) also runs on the main thread.

6. **Outcome shape.** Each task returns a `SummaryOutcome`
   (`thread_id`, `summary_text | None`, `error | None`, `skipped: bool`,
   carrying `job` and its `metrics`) so the main thread can finalize DB state
   and merge telemetry without re-calling the model.

7. **Interrupt handling reuses the embed registry.** `EmailThreadsSummaryDispatcher`
   registers in the *same* module-level weakref set (`_LIVE`) exported by
   `modules/embedding/dispatch.py` that `EmbedDispatcher` uses, so the existing
   `cancel_all()` (wired into `cli.py`'s interrupt paths) already abandons
   in-flight generation futures. No new `cli.py` call sites are added. An
   interrupt leaves generated-but-uncommitted work as durable pending gaps;
   the next `ingest all` re-summarizes stale threads.

8. **Unavailable degradation mirrors embeddings.** If oMLX is unreachable
   (`InferenceUnavailable`), the dispatcher marks itself unavailable after the
   first failure, stops submitting, and reports every remaining thread as
   `skipped` (not `failed`) — a durable pending gap, not a review error. This
   matches `EmbedDispatcher._mark_unavailable`.

9. **Telemetry is merged, never written from workers.** Per-task
   `metrics`/`timings` are accumulated locally on the worker and folded into
   `ctx.telemetry.summaries` on the main thread during `drain()`. No shared
   telemetry object is mutated from a worker thread.

10. **`Progress` is driven only on the main thread.** The `progress.start(...)`
    call currently inside `_call_generator` (`summaries.py`) is removed from
    the worker path; the dispatcher records a human-readable `note` at submit
    time (e.g. `thread N · one-shot` / `thread N · hierarchical`) and the main
    thread issues `progress.step(note=...)` as each future resolves.

## The two workloads, side by side

| Concern | Dispatcher | Off-thread work | Main-thread settlement |
|---|---|---|---|
| Email/PDF embeddings | `EmbedDispatcher` | `embed(text)` → `atomic_publish_array` | matrix rebuild |
| Summary embeddings | `EmbedDispatcher` (`submit_summary`) | `embed(summary)` → `atomic_publish_array` | matrix rebuild |
| **Summary generation** | **`EmailThreadsSummaryDispatcher`** | **`generate(body, mode)`** | **DB upsert + commit + `submit_summary`** |

## Generation concurrency shape

```
producers (emails/pdfs) ──► EmbedDispatcher (8-wide) ──► oMLX /embeddings
summaries stage:
   for job in stale: dispatcher.submit(job)        # non-blocking
   done, failed, skipped, outcomes = dispatcher.drain(progress)
   for outcome in outcomes:                         # main thread
       write DB row + commit
       embed_dispatcher.submit_summary(...)        # 8-wide, already concurrent
```

Each thread's `_generate` runs on a worker; the stage no longer waits on any
single generation before starting the next. Eight threads generate
concurrently → the ~7-minute serial pass collapses toward ~1 minute on the
same hardware.

## Call sites affected

- `modules/pipeline/summaries.py` — the `run` generation loop
  (~lines 211–279) is rewritten to submit-then-drain; `_generate`,
  `_call_generator`, `_reduce`, `_structural_segments` move (unchanged in
  logic) into the dispatcher or remain on the stage and are passed `generator`.
  The inner `progress.start` is removed from the worker path.
- `modules/pipeline/summary_dispatch.py` (new) — `EmailThreadsSummaryDispatcher`,
  `SummaryOutcome`, and registration in the shared `_LIVE` set.
- `modules/embedding/dispatch.py` — export `_LIVE` (or a `register` helper) so
  the new dispatcher shares the interrupt-abandon registry; `cancel_all()`
  needs no other change.
- `modules/cli.py` — no change (shared registry covers interrupt).
- `modules/inference.py` — no change (`INFERENCE_MAX_IN_FLIGHT` reused as-is).
- `modules/config.py` / `config.yaml` — no change this pass (width uses the
  constant directly).

## Verification

- Tests in `modules/tests/test_summary_performance.py` (and any summary test):
  with a fake `generate` and `max_in_flight < len(stale)`, assert (a) all
  threads complete, (b) DB rows are written on the main thread, (c)
  `InferenceUnavailable` → all `skipped` not `failed`, (d) a per-thread
  exception → that thread flagged and others succeed, (e) ≥2 `generate` calls
  overlap (proven via a `threading.Event`).
- Full suite: `for test_file in modules/tests/test_*.py; do uv run python
  "$test_file"; done` and `uv run ./pocket-advisor.py test` — all green.
- `git diff --check` + `git status --short` clean.
- Manual: `ingest all` on `case-documents-demo` shows `generate thread
  summaries` advancing ~8× faster with no "database is locked" and no
  garbled progress; rerun is idempotent (stale threads only).

## Open items (deferred)

- **Split `summary_max_in_flight`** (likely 4) from embedding width for 4B
  decode memory/throughput headroom.
- **Intra-thread segment fan-out** — parallelize the independent segment calls
  within hierarchical threads (second-level concurrency).
- **Auto-spawn oMLX if down** — explicitly out of scope; the design refuses to
  manage the server lifecycle.
