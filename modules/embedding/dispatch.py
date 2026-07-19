"""Readiness-time embedding dispatch and convergence fan-out.

Design decision 5 (`docs/features/embedding-design-v2.md`): the moment a
producer publishes an inference-ready artifact its embedding payloads are
dispatched asynchronously to the inference endpoint. There is no internal
queue — a bounded pool keeps at most ``INFERENCE_MAX_IN_FLIGHT`` requests
open (matching oMLX's continuous-batching concurrency) and a saturated pool
gives the submitting producer backpressure.

Producer dispatch is best-effort: an unreachable endpoint prints one
warning, stops dispatching for the run, and leaves entities pending —
evidence custody never depends on inference availability. The embed stage
is the loud, authoritative convergence pass over the same dispatcher.

Every published vector follows the atomic write-verify-publish discipline,
so an interrupt at any point leaves durable gaps the next `ingest embed`
fills.
"""
import threading
import time
import weakref
from concurrent.futures import Future, ThreadPoolExecutor
from dataclasses import dataclass
from pathlib import Path
from typing import Protocol

from modules.embedding.backends import (atomic_publish_array,
                                        current_fingerprint, get_backend,
                                        index_paths, thread_index_paths,
                                        thread_vector_filename,
                                        validated_vector)
from modules.inference import (INFERENCE_MAX_IN_FLIGHT, InferenceUnavailable,
                               estimate_tokens)
from modules.telemetry import NOT_RUN, PARTIAL


class LiveDispatcher(Protocol):
    """Any dispatcher that holds in-flight inference work and can abandon
    its queued items on interrupt. Both ``EmbedDispatcher`` and
    ``EmailThreadsSummaryDispatcher`` register here so ``cancel_all()``
    covers every inference fan-out through one hook."""

    def abandon(self) -> None: ...


# Live dispatchers, so an interrupted or failed run can abandon queued
# work instead of blocking process exit on thousands of pending requests.
_LIVE: "weakref.WeakSet[LiveDispatcher]" = weakref.WeakSet()


def shared_dispatcher(ctx) -> "EmbedDispatcher":
    """The run-wide dispatcher producers submit to without waiting."""
    if ctx.embed_dispatcher is None:
        ctx.embed_dispatcher = EmbedDispatcher(ctx)
    return ctx.embed_dispatcher


def cancel_all() -> None:
    """Abandon every live dispatcher's queued work (interrupt/failure
    path): in-flight requests finish, queued ones are dropped as durable
    pending gaps for the next `ingest embed`."""
    for dispatcher in list(_LIVE):
        dispatcher.abandon()


def drain_leftover(ctx) -> None:
    """End-of-run settlement for a pipeline that never reached the embed
    stage (named-prefix runs): wait for in-flight readiness dispatches so
    their vectors become durable, then report plainly."""
    dispatcher = getattr(ctx, "embed_dispatcher", None)
    if dispatcher is None:
        return
    pending = dispatcher.pending_count
    if pending:
        print(f"embedding: waiting for {pending} in-flight readiness"
              " dispatches…")
    done, failed, skipped, _ = dispatcher.drain()
    if done or failed or skipped:
        print(f"embedding: {done} published, {failed} failed,"
              f" {skipped} left pending (readiness dispatch)")
    if dispatcher.unavailable is not None and (failed or skipped):
        print(f"embedding: {failed + skipped} entities left un-embedded —"
              " run 'ingest embed' after starting oMLX")
    dispatcher.close()
    ctx.embed_dispatcher = None


@dataclass(frozen=True, slots=True)
class DispatchOutcome:
    review_key: str
    note: str
    error: str | None      # non-None: this entity failed and was flagged
    skipped: bool          # endpoint down — left pending, not failed


class EmbedDispatcher:
    """Bounded async fan-out from payload text to published vector."""

    def __init__(self, ctx, *, backend=None, fingerprint=None):
        self.config = ctx.config
        self.telemetry = ctx.telemetry.embed
        self.fingerprint = fingerprint if fingerprint is not None \
            else current_fingerprint(ctx.config)
        # Directories are created on first publication
        # (atomic_publish_array), never eagerly — a run that dispatches
        # nothing must leave no empty cache directories behind.
        self.leaf_paths = index_paths(ctx.config, self.fingerprint)
        self.thread_paths = thread_index_paths(ctx.config, self.fingerprint)
        self.backend = backend if backend is not None \
            else get_backend(ctx.config)
        self.unavailable: str | None = None
        self._lock = threading.Lock()
        self._pool: ThreadPoolExecutor | None = None
        self._futures: list[Future] = []
        _LIVE.add(self)

    @property
    def pending_count(self) -> int:
        return sum(1 for future in self._futures if not future.done())

    # -- submission --------------------------------------------------------

    def submit_leaf(self, chunk_id: int, payload: str, *,
                    at_readiness: bool = False) -> bool:
        target = self.leaf_paths.vecs_dir / f"{chunk_id}.npy"
        return self._submit(payload, target, "leaf",
                            f"chunk:{chunk_id}", f"chunk {chunk_id}",
                            at_readiness)

    def submit_summary(self, thread_id: int, summary_text: str, *,
                       at_readiness: bool = False) -> bool:
        target = self.thread_paths.vecs_dir / thread_vector_filename(
            thread_id, summary_text)
        return self._submit(summary_text, target, "summary",
                            f"thread:{thread_id}", f"thread {thread_id}",
                            at_readiness)

    def submit_pending_leaves(self, conn, *, source_type: str | None = None,
                              document_id: int | None = None,
                              at_readiness: bool = False) -> int:
        """Dispatch every chunk without a vector in the current cache."""
        sql = ("SELECT id, payload_shadow FROM chunks"
               " WHERE payload_shadow IS NOT NULL")
        params: list = []
        if source_type is not None:
            sql += " AND source_type = ?"
            params.append(source_type)
        if document_id is not None:
            sql += " AND document_id = ?"
            params.append(document_id)
        sql += " ORDER BY id"
        submitted = 0
        for row in conn.execute(sql, params).fetchall():
            if self.submit_leaf(int(row["id"]), row["payload_shadow"],
                                at_readiness=at_readiness):
                submitted += 1
        return submitted

    def _submit(self, text: str, target: Path, queue_name: str,
                review_key: str, note: str, at_readiness: bool) -> bool:
        if self.unavailable is not None or target.is_file():
            return False
        if self._pool is None:
            self._pool = ThreadPoolExecutor(
                max_workers=INFERENCE_MAX_IN_FLIGHT,
                thread_name_prefix="embed-dispatch")
        if at_readiness:
            queue = getattr(self.telemetry.queues, queue_name)
            with self._lock:
                self._mark_entered()
                queue.dispatched_at_readiness += 1
        self._futures.append(self._pool.submit(
            self._task, text, target, queue_name, review_key, note))
        return True

    # -- execution ---------------------------------------------------------

    def _task(self, text: str, target: Path, queue_name: str,
              review_key: str, note: str) -> DispatchOutcome:
        if self.unavailable is not None:
            return DispatchOutcome(review_key, note, None, True)
        queue = getattr(self.telemetry.queues, queue_name)
        started = time.monotonic()
        try:
            embed = getattr(self.backend, "embed_with_usage", None)
            if embed is not None:
                vector, tokens = embed(text)
            else:
                vector = self.backend.embed_one(text)
                tokens = estimate_tokens(text)
            checked = validated_vector(vector, self.fingerprint["dim"])
        except InferenceUnavailable as exc:
            self._mark_unavailable(str(exc))
            return DispatchOutcome(review_key, note, None, True)
        except Exception as exc:
            with self._lock:
                self._mark_entered()
                self.telemetry.timings_seconds.model_execution += \
                    time.monotonic() - started
                queue.pending_entities += 1
                queue.failed_entities += 1
            return DispatchOutcome(
                review_key, note, f"{type(exc).__name__}: {exc}", False)
        model_seconds = time.monotonic() - started
        publish_started = time.monotonic()
        try:
            atomic_publish_array(target, checked)
        except Exception as exc:
            with self._lock:
                self._mark_entered()
                self.telemetry.timings_seconds.model_execution += \
                    model_seconds
                queue.pending_entities += 1
                queue.failed_entities += 1
            return DispatchOutcome(
                review_key, note, f"{type(exc).__name__}: {exc}", False)
        with self._lock:
            self._mark_entered()
            timings = self.telemetry.timings_seconds
            timings.model_execution += model_seconds
            timings.cache_publication += time.monotonic() - publish_started
            queue.pending_entities += 1
            queue.successful_entities += 1
            queue.input_tokens += tokens
            self.telemetry.verified_cache_publications += 1
        return DispatchOutcome(review_key, note, None, False)

    def _mark_entered(self) -> None:
        """Readiness dispatch attributes work to the shared embed record;
        a not_run state may not carry counters, so the first touch marks
        the record partial (sealed measured by the embed stage)."""
        if self.telemetry.state == NOT_RUN:
            self.telemetry.state = PARTIAL

    def _mark_unavailable(self, message: str) -> None:
        with self._lock:
            if self.unavailable is not None:
                return
            self.unavailable = message
        print(f"embed dispatch: {message}")

    # -- draining ----------------------------------------------------------

    def drain(self, progress=None) -> tuple[int, int, int,
                                            list[DispatchOutcome]]:
        """Wait for every in-flight dispatch. Returns
        (published, failed, left_pending, outcomes)."""
        futures, self._futures = self._futures, []
        done = failed = skipped = 0
        outcomes: list[DispatchOutcome] = []
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
        """Drop queued dispatches without waiting; in-flight requests
        finish and publish. Everything dropped is a durable pending gap."""
        self._futures = []
        if self._pool is not None:
            self._pool.shutdown(wait=False, cancel_futures=True)
            self._pool = None

    def close(self) -> None:
        if self._pool is not None:
            self._pool.shutdown(wait=True)
            self._pool = None
