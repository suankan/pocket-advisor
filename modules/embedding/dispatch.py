"""Readiness-time embedding dispatch and convergence fan-out.

Design decision 5 (`docs/inference/inference-serving.md`): the moment a
producer publishes an inference-ready artifact its embedding payloads are
dispatched asynchronously to the inference endpoint. A bounded pool keeps at
most ``INFERENCE_MAX_IN_FLIGHT`` requests in flight, matching oMLX's
continuous-batching concurrency; queued work above that is not bounded and
does not block the producer, which submits and moves on
(`docs/ingestion/embedding-queue-and-workers.md` decision 7).

Leaf chunks and thread summaries share this one queue. The ``leaf``/
``summary`` split is a telemetry label selecting a counter bucket, not a
second pool.

Producer dispatch is best-effort: an unreachable endpoint prints one
warning, stops dispatching for the run, and leaves entities pending —
content integrity never depends on inference availability. The embed stage
is the loud, authoritative convergence pass over the same dispatcher.

Every published vector follows the atomic write-verify-publish discipline,
so an interrupt at any point leaves durable gaps the next `ingest embed`
fills.
"""
import time
from dataclasses import dataclass
from pathlib import Path

from modules.chunk_reader import ChunkReader
from modules.embedding.backends import (atomic_publish_array,
                                        current_fingerprint, get_backend,
                                        index_paths, thread_index_paths,
                                        thread_vector_filename,
                                        validated_vector)
from modules.embedding.chunks import CHUNK_ENVELOPE_SQL, chunk_payload
from modules.dispatch import BoundedInferenceDispatcher
from modules.inference import (INFERENCE_MAX_IN_FLIGHT, InferenceUnavailable,
                               estimate_tokens)
from modules.logs import get_log
from modules.telemetry import NOT_RUN, PARTIAL


def shared_dispatcher(ctx) -> "EmbedDispatcher":
    """The run-wide dispatcher producers submit to without waiting."""
    if ctx.embed_dispatcher is None:
        ctx.embed_dispatcher = EmbedDispatcher(ctx)
    return ctx.embed_dispatcher


def drain_leftover(ctx) -> None:
    """End-of-run settlement for a pipeline that never reached the embed
    stage (named-prefix runs): wait for in-flight readiness dispatches so
    their vectors become durable, then report plainly."""
    dispatcher = getattr(ctx, "embed_dispatcher", None)
    if dispatcher is None:
        return
    pending = dispatcher.pending_count
    if pending:
        get_log().notice(
            f"embedding: waiting for {pending} in-flight readiness"
            " dispatches…", pending_dispatches=pending)
    done, failed, skipped, _ = dispatcher.drain()
    if done or failed or skipped:
        get_log().notice(
            f"embedding: {done} published, {failed} failed,"
            f" {skipped} left pending (readiness dispatch)",
            published=done, failed=failed, skipped=skipped)
    if dispatcher.unavailable is not None and (failed or skipped):
        get_log().error(
            f"embedding: {failed + skipped} entities left un-embedded —"
            " run 'ingest embed' after starting oMLX",
            un_embedded=failed + skipped, reason=dispatcher.unavailable)
    dispatcher.close()
    ctx.embed_dispatcher = None


@dataclass(frozen=True, slots=True)
class DispatchOutcome:
    review_key: str
    note: str
    error: str | None      # non-None: this entity failed and was flagged
    skipped: bool          # endpoint down — left pending, not failed


class EmbedDispatcher(BoundedInferenceDispatcher):
    """Bounded async fan-out from payload text to published vector."""

    thread_name_prefix = "embed-dispatch"
    unavailable_label = "embed dispatch"
    queue_label = "embed queue"

    def __init__(self, ctx, *, backend=None, fingerprint=None):
        super().__init__(max_in_flight=INFERENCE_MAX_IN_FLIGHT)
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

    def retarget(self, *, backend, fingerprint: dict) -> None:
        """Point the run's one dispatcher at the readiness-verified backend
        and the embed stage's fingerprint before the convergence pass.

        Legal only while idle. The caller has just drained its barrier;
        retargeting with work in flight would publish in-progress vectors
        into the wrong cache directory
        (`docs/ingestion/embedding-queue-and-workers.md` acceptance
        criterion 4).
        """
        snapshot = self.snapshot()
        if not snapshot.idle:
            raise RuntimeError(
                "retarget requires an idle dispatcher: "
                f"{snapshot.queued} queued, {snapshot.in_flight} in flight")
        self.backend = backend
        self.fingerprint = fingerprint
        self.leaf_paths = index_paths(self.config, fingerprint)
        self.thread_paths = thread_index_paths(self.config, fingerprint)

    # -- submission --------------------------------------------------------

    def submit_leaf(self, chunk_id: int, payload: str, *,
                    at_readiness: bool = False) -> bool:
        target = self.leaf_paths.vecs_dir / f"{chunk_id}.npy"
        return self._submit(payload, target, "leaf",
                            f"chunk:{chunk_id}", f"chunk {chunk_id}",
                            at_readiness)

    def submit_summary(self, thread_id: int, summary_text: str,
                       summary_sha256: str, *,
                       at_readiness: bool = False) -> bool:
        target = self.thread_paths.vecs_dir / thread_vector_filename(
            thread_id, summary_sha256)
        return self._submit(summary_text, target, "summary",
                            f"thread:{thread_id}", f"thread {thread_id}",
                            at_readiness)

    def submit_pending_leaves(self, conn, *, source_type: str | None = None,
                              document_id: int | None = None,
                              email_ids: set[int] | None = None,
                              at_readiness: bool = False) -> int:
        """Dispatch every chunk without a vector in the current cache.
        Payloads are derived on demand — chunk slice through the reader
        plus the relational envelope — and only for actually-pending
        chunks, so a converged corpus reads no artifacts here."""
        sql = CHUNK_ENVELOPE_SQL
        conds: list[str] = []
        params: list = []
        if source_type is not None:
            conds.append("chunks.source_type = ?")
            params.append(source_type)
        if document_id is not None:
            conds.append("chunks.document_id = ?")
            params.append(document_id)
        if email_ids is not None:
            if not email_ids:
                return 0
            marks = ",".join("?" for _ in email_ids)
            conds.append(f"chunks.email_id IN ({marks})")
            params.extend(sorted(email_ids))
        if conds:
            sql += " WHERE " + " AND ".join(conds)
        sql += " ORDER BY chunks.id"
        reader = ChunkReader(conn, self.config)
        submitted = 0
        for row in conn.execute(sql, params).fetchall():
            if self.unavailable is not None:
                break
            if (self.leaf_paths.vecs_dir / f"{row['id']}.npy").is_file():
                continue
            payload = chunk_payload(row, reader.chunk_text(row))
            if self.submit_leaf(int(row["id"]), payload,
                                at_readiness=at_readiness):
                submitted += 1
        return submitted

    def _submit(self, text: str, target: Path, queue_name: str,
                review_key: str, note: str, at_readiness: bool) -> bool:
        if self.unavailable is not None or target.is_file():
            return False
        if at_readiness:
            queue = getattr(self.telemetry.queues, queue_name)
            with self._lock:
                self._mark_entered()
                queue.dispatched_at_readiness += 1
        self._submit_task(
            self._task, text, target, queue_name, review_key, note)
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
                queue.processed_entities += 1
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
                queue.processed_entities += 1
                queue.failed_entities += 1
            return DispatchOutcome(
                review_key, note, f"{type(exc).__name__}: {exc}", False)
        with self._lock:
            self._mark_entered()
            timings = self.telemetry.timings_seconds
            timings.model_execution += model_seconds
            timings.cache_publication += time.monotonic() - publish_started
            queue.processed_entities += 1
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
