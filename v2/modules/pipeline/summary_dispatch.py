"""Bounded concurrent generation of email-thread summaries.

Companion to ``modules/embedding/dispatch.py`` (which fans out *embeddings*).
This module fans out the summarization *generation* loop: each stale thread's
``generate`` call to the oMLX inference server runs on a worker thread inside a
bounded pool, so up to ``INFERENCE_MAX_IN_FLIGHT`` threads decode
concurrently instead of one at a time.

Design (``docs/ingestion/summary-generation-concurrency.md``):

* Only the inference call runs off-thread. The worker mutates its own
  per-task ``_GenerationMetrics`` and a fresh ``SummariesTimings``; it never
  touches the database connection, ``Progress``, or ``ReviewLog``.
* Settlement — the DB upsert, commit, summary-embed dispatch, telemetry
  merge, and progress bar — runs on the main thread as ``drain()`` observes
  each completion.
* The pool, availability latch, and drain/abandon/close lifecycle come from
  the shared ``BoundedInferenceDispatcher`` (``modules/dispatch.py``), whose
  registry lets a process interrupt abandon in-flight generation through the
  one ``cancel_all()`` hook. The pool itself stays separate from the embed
  dispatcher's: the two endpoints are independent capacity budgets.

The generation core (``_generate`` and its helpers) lives here as module-level
functions so workers have a clean, progress-free execution path; the stage
passes the ``SummaryGenerator`` in.
"""
from dataclasses import dataclass

from v2.modules.dispatch import BoundedInferenceDispatcher
from v2.modules.inference import INFERENCE_MAX_IN_FLIGHT, InferenceUnavailable
from v2.modules.pipeline.summaries_core import (
    _GenerationMetrics, _ThreadWork, _generate_thread_summary)
from v2.modules.progress import Progress
from v2.modules.telemetry import SummariesTimings


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


class EmailThreadsSummaryDispatcher(BoundedInferenceDispatcher):
    """Bounded async fan-out from stale threads to generated summaries."""

    thread_name_prefix = "summary-gen"
    unavailable_label = "summary generation"
    queue_label = "summary queue"

    def __init__(self, ctx, generator, *,
                 max_in_flight: int = INFERENCE_MAX_IN_FLIGHT):
        super().__init__(max_in_flight=max_in_flight)
        self.config = ctx.config
        self.telemetry = ctx.telemetry.summaries
        self.generator = generator

    def submit(self, job: "_ThreadWork") -> bool:
        """Queue one thread's generation. Returns False if the endpoint is
        already known down."""
        if self.unavailable is not None:
            return False
        self._submit_task(
            self._task, job, _GenerationMetrics(), SummariesTimings())
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

    # drain/abandon/close come from BoundedInferenceDispatcher. Abandoning
    # drops queued generations; in-flight requests finish but their
    # settlement is skipped — durable pending gaps for the next `ingest all`.
