"""Bounded concurrent generation of email-thread summaries.

Companion to ``modules/embedding/dispatch.py`` (which fans out *embeddings*).
This module fans out the summarization *generation* loop: each stale thread's
``generate`` call to the oMLX inference server runs on a worker thread inside a
bounded pool, so up to ``INFERENCE_MAX_IN_FLIGHT`` threads decode
concurrently instead of one at a time.

Design (``docs/features/summary-generation-concurrency.md``):

* Only the inference call runs off-thread. The worker mutates its own
  per-task ``_GenerationMetrics`` and a fresh ``SummariesTimings``; it never
  touches the database connection, ``Progress``, or ``ReviewLog``.
* Settlement — the DB upsert, commit, summary-embed dispatch, telemetry
  merge, and progress bar — runs on the main thread after ``drain()``.
* The dispatcher shares the embed dispatcher's weakref registry
  (``modules/embedding/dispatch``) so a process interrupt abandons in-flight
  generation through the existing ``cancel_all()`` hook.

The generation core (``_generate`` and its helpers) lives here as module-level
functions so workers have a clean, progress-free execution path; the stage
passes the ``SummaryGenerator`` in.
"""
from concurrent.futures import Future, ThreadPoolExecutor
from dataclasses import dataclass

from modules.embedding.dispatch import _LIVE
from modules.inference import INFERENCE_MAX_IN_FLIGHT, InferenceUnavailable
from modules.pipeline.summaries_core import (
    _GenerationMetrics, _ThreadWork, _generate_thread_summary)
from modules.progress import Progress
from modules.telemetry import SummariesTimings


@dataclass(frozen=True, slots=True)
class SummaryOutcome:
    thread_id: int
    stable_key: str
    job: "_ThreadWork"
    metrics: "_GenerationMetrics"
    timings: "SummariesTimings"
    note: str
    summary_text: str | None = None
    error: str | None = None
    skipped: bool = False


class EmailThreadsSummaryDispatcher:
    """Bounded async fan-out from stale threads to generated summaries."""

    def __init__(self, ctx, generator, *,
                 max_in_flight: int = INFERENCE_MAX_IN_FLIGHT):
        self.config = ctx.config
        self.telemetry = ctx.telemetry.summaries
        self.generator = generator
        self.unavailable: str | None = None
        self._lock = None  # unused placeholder for symmetry; not needed
        self._pool: ThreadPoolExecutor | None = None
        self._max = max(1, max_in_flight)
        self._futures: list[Future] = []
        _LIVE.add(self)

    @property
    def pending_count(self) -> int:
        return sum(1 for future in self._futures if not future.done())

    def submit(self, job: "_ThreadWork") -> bool:
        """Queue one thread's generation. Returns False if the endpoint is
        already known down or the pool failed to start."""
        if self.unavailable is not None:
            return False
        if self._pool is None:
            self._pool = ThreadPoolExecutor(
                max_workers=self._max,
                thread_name_prefix="summary-gen")
        metrics = _GenerationMetrics()
        timings = SummariesTimings()
        self._futures.append(self._pool.submit(
            self._task, job, metrics, timings))
        return True

    def _task(self, job: "_ThreadWork", metrics: "_GenerationMetrics",
              timings: "SummariesTimings") -> SummaryOutcome:
        if self.unavailable is not None:
            return SummaryOutcome(
                job.thread_id, job.stable_key, job, metrics, timings,
                f"thread {job.thread_id}", skipped=True)
        try:
            summary_text, note = _generate_thread_summary(
                self.generator, job, metrics, timings)
        except InferenceUnavailable as exc:
            self._mark_unavailable(str(exc))
            return SummaryOutcome(
                job.thread_id, job.stable_key, job, metrics, timings,
                f"thread {job.thread_id}", skipped=True)
        except Exception as exc:
            return SummaryOutcome(
                job.thread_id, job.stable_key, job, metrics, timings,
                f"thread {job.thread_id}",
                error=f"{type(exc).__name__}: {exc}")
        return SummaryOutcome(
            job.thread_id, job.stable_key, job, metrics, timings, note,
            summary_text=summary_text)

    def _mark_unavailable(self, message: str) -> None:
        if self.unavailable is not None:
            return
        self.unavailable = message
        print(f"summary generation: {message}")

    def drain(self, progress: Progress | None = None
              ) -> tuple[int, int, int, list[SummaryOutcome]]:
        """Wait for every in-flight generation. Returns
        (done, failed, skipped, outcomes) in submission order."""
        futures, self._futures = self._futures, []
        done = failed = skipped = 0
        outcomes: list[SummaryOutcome] = []
        try:
            for future in futures:
                outcome = future.result()
                outcomes.append(outcome)
                if progress is not None:
                    progress.step(note=outcome.note)
                if outcome.skipped:
                    skipped += 1
                elif outcome.error is not None:
                    failed += 1
                else:
                    done += 1
        except BaseException:
            if self._pool is not None:
                self._pool.shutdown(wait=False, cancel_futures=True)
                self._pool = None
            raise
        return done, failed, skipped, outcomes

    def abandon(self) -> None:
        """Drop queued generations without waiting; in-flight requests
        finish and publish their summaries, but settlement is skipped —
        durable pending gaps for the next `ingest all`."""
        self._futures = []
        if self._pool is not None:
            self._pool.shutdown(wait=False, cancel_futures=True)
            self._pool = None

    def close(self) -> None:
        if self._pool is not None:
            self._pool.shutdown(wait=True)
            self._pool = None
