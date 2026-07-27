"""SummarisationEmbeddingService — one thread in, one embedded summary out.

Given a thread id and its ordered message artifacts, generate the navigation
summary at the summarisation endpoint, write
`thread-summaries/<thread_id>.txt`, embed the summary text, and publish its
vector. The answer carries the summary text and its digest so the hub can write
`thread_summaries` and its FTS row.

Generation and its embedding are one unit here, on one worker. The embed call
is a rounding error beside a multi-second generation, and keeping them together
means a thread's summary and its vector settle or fail as one thing. The pool
stays sized to the *summarisation* endpoint's in-flight budget, separate from
the plain-text embedding budget, so slow generations cannot starve leaf
embedding (`embedding-queue-and-workers.md` decision 2).

Summary vectors come from the **embedding** model, not the summarisation
model: they share a vector space with leaf chunks by construction, and
embedding them with anything else would make thread and leaf scores
incomparable.

Design: `docs/ingestion/document-flow-services.md` D7.
"""
from __future__ import annotations

import threading
from pathlib import Path
from typing import Any

from modules.config import Config
from modules.embedding.backends import thread_vector_filename
from modules.embedding.dispatch import EmbedDispatcher
from modules.inference import INFERENCE_MAX_IN_FLIGHT, InferenceUnavailable
from modules.integrity import write_verified
from modules.pipeline.summaries_core import (_GenerationMetrics,
                                             _MessageSource, _ThreadWork,
                                             _generate_thread_summary)
from modules.services.base import ItemResult, QueueBackedService
from modules.summarization import get_summary_generator
from modules.telemetry import SummariesTimings


class SummarisationEmbeddingService(QueueBackedService):
    """Bounded generation of thread summaries, embedded on the way out."""

    name = "summarisation-embedding"
    detail = "summarise · embed · publish"

    def __init__(self, config: Config, telemetry, *, log=None,
                 workers: int | None = None):
        super().__init__(
            log=log,
            workers=workers if workers is not None
            else INFERENCE_MAX_IN_FLIGHT)
        self.config = config
        self.generator = get_summary_generator(config)
        self.engine = EmbedDispatcher(config, telemetry)
        self._unavailable: str | None = None
        self._latch = threading.Lock()

    @property
    def unavailable(self) -> str | None:
        """The generation endpoint-down latch, held once for the run."""
        with self._latch:
            return self._unavailable

    # -- Service ----------------------------------------------------------

    def handle(self, item: dict[str, Any]) -> ItemResult:
        job = _job_from(item)
        note = f"thread {job.thread_id}"
        if self.unavailable is not None:
            return ItemResult(payload={"thread_id": job.thread_id},
                              note=note, skipped=True)
        metrics = _GenerationMetrics()
        timings = SummariesTimings()
        try:
            summary_text, note = _generate_thread_summary(
                self.generator, job, metrics, timings)
        except InferenceUnavailable as exc:
            self._mark_unavailable(str(exc))
            return ItemResult(payload={"thread_id": job.thread_id},
                              note=note, skipped=True)
        except Exception as exc:
            return ItemResult(
                payload={"thread_id": job.thread_id,
                         **_measurements(metrics, timings)},
                note=note, error=f"{type(exc).__name__}: {exc}")

        # Artifact first, then the row: a crash in between leaves a current
        # file with a stale row, which the next run's digest check regenerates.
        summary_sha256 = write_verified(
            self.config.summary_path(job.thread_id),
            summary_text.encode("utf-8"))
        self._embed(job.thread_id, summary_text, summary_sha256, note)
        return ItemResult(
            payload={
                "thread_id": job.thread_id,
                "summary_text": summary_text,
                "summary_sha256": summary_sha256,
                **_measurements(metrics, timings),
            },
            note=note)

    def close(self) -> None:
        super().close()
        self.engine.close()

    def abort(self) -> None:
        super().abort()
        self.engine.abandon()

    # -- work ---------------------------------------------------------------

    def _embed(self, thread_id: int, summary_text: str, summary_sha256: str,
               note: str) -> None:
        """Publish the summary's vector. Best effort, like every producer
        dispatch: a missing vector is a durable pending gap the embed stage
        converges, while a missing summary would be lost work."""
        target = Path(self.engine.thread_paths.vecs_dir) / \
            thread_vector_filename(thread_id, summary_sha256)
        if target.is_file():
            return
        outcome = self.engine.execute(
            summary_text, target, "summary", f"thread:{thread_id}", note)
        if outcome.error is not None:
            self.log.info(
                f"{self.name}: summary vector deferred for thread"
                f" {thread_id}: {outcome.error}",
                service=self.name, thread_id=thread_id)

    def _mark_unavailable(self, message: str) -> None:
        with self._latch:
            if self._unavailable is not None:
                return
            self._unavailable = message
        self.log.error(f"summary generation: {message}", service=self.name)


def _job_from(item: dict[str, Any]) -> _ThreadWork:
    return _ThreadWork(
        thread_id=int(item["thread_id"]),
        stable_key=str(item["stable_key"]),
        source_digest=str(item["source_digest"]),
        messages=tuple(
            _MessageSource(str(message["message_id"]),
                           str(message["date_utc"]),
                           Path(str(message["path"])))
            for message in item["messages"]),
    )


def _measurements(metrics: _GenerationMetrics,
                  timings: SummariesTimings) -> dict[str, Any]:
    return {
        "metrics": {
            "source_tokens": metrics.source_tokens,
            "input_tokens": metrics.input_tokens,
            "segments": metrics.segments,
            "calls": metrics.calls,
            "reductions": metrics.reductions,
            "strategy": metrics.strategy,
        },
        "timings": {
            "input_render": timings.input_render,
            "model_execution": timings.model_execution,
        },
    }
