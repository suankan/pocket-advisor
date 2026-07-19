"""Stage 4 — chunk the plain-text artifacts and embed via Jina MLX.

Inputs are exactly the Stage 2/3 text artifacts, read through the DB:
- emails.body_text_path — authored email messages
  (chunk source_type 'email_body', email_id set)
- documents.extracted_text_path — PDF-to-text artifacts, one per unique
  document regardless of how many times it occurs
  (chunk source_type 'document_text', document_id set)

Chunks ~chunk_chars with ~chunk_overlap, splitting on paragraph
boundaries where possible; every artifact yields >=1 chunk so every
email/document is individually citable. Chunks are immutable once
created.

Incremental: a chunk is pending for the CURRENT model whenever its id
has no <vecs_dir>/<id>.npy yet — the per-chunk cache is the durable
source of truth, so a crash mid-run loses at most one chunk, and a
failed chunk is retried next run. Each run rebuilds vectors.npy +
vectors_ids.npy from that cache.
"""
import json
import os
import tempfile
import time
from collections.abc import Iterator
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path

import numpy as np

from modules.custody import write_verified
from modules.domain import StageStats
from modules.emailbody import body_text as message_body_text
from modules.embedding import (EMBED_BATCH_SIZE, EMBED_BUCKET_WIDTH,
                               EMBED_MAX_TOKENS, IndexPaths, ModelStore,
                               chunking_fields_changed, current_fingerprint,
                               enriched_payload, get_backend, index_paths,
                               meta_fingerprint, thread_index_paths,
                               thread_vector_filename)
from modules.pipeline.base import Stage
from modules.progress import Progress
from modules.transliteration import proper_noun_shadow


@dataclass(frozen=True, slots=True)
class _PendingEmbedding:
    entity_id: int
    text: str
    target: Path
    note: str
    review_key: str


def _validated_vector(vector, dim: int) -> np.ndarray:
    result = np.asarray(vector, dtype=np.float32).reshape(-1)
    if result.shape != (dim,):
        raise ValueError(
            f"embedding shape {result.shape} != expected ({dim},)")
    if not np.isfinite(result).all():
        raise ValueError("embedding contains non-finite values")
    return result


def _atomic_publish_array(target: Path, array: np.ndarray) -> None:
    """Write, read-verify, then atomically publish one numpy artifact."""
    target.parent.mkdir(parents=True, exist_ok=True)
    fd, raw_temp = tempfile.mkstemp(
        prefix=f".{target.name}.", suffix=".tmp", dir=target.parent)
    os.close(fd)
    temp = Path(raw_temp)
    try:
        with temp.open("wb") as handle:
            np.save(handle, array, allow_pickle=False)
        observed = np.load(temp, allow_pickle=False)
        if observed.dtype != array.dtype or observed.shape != array.shape \
                or not np.array_equal(observed, array, equal_nan=True):
            raise OSError(f"numpy write verification failed for {target}")
        os.replace(temp, target)
    finally:
        temp.unlink(missing_ok=True)


def _atomic_publish_json(target: Path, value: dict) -> None:
    target.parent.mkdir(parents=True, exist_ok=True)
    fd, raw_temp = tempfile.mkstemp(
        prefix=f".{target.name}.", suffix=".tmp", dir=target.parent)
    os.close(fd)
    temp = Path(raw_temp)
    try:
        write_verified(temp, json.dumps(value, indent=2).encode("utf-8"))
        os.replace(temp, target)
    finally:
        temp.unlink(missing_ok=True)


def chunk_text(text: str, chunk_chars: int,
               chunk_overlap: int) -> Iterator[tuple[int, int, int, str]]:
    """Yield (chunk_index, char_start, char_end, chunk)."""
    content_start = len(text) - len(text.lstrip())
    content_end = len(text.rstrip())
    if content_start >= content_end:
        return
    if content_end - content_start <= chunk_chars:
        yield 0, content_start, content_end, text[content_start:content_end]
        return
    idx, start = 0, content_start
    while start < content_end:
        end = min(start + chunk_chars, content_end)
        if end < content_end:
            # prefer a paragraph break, then any newline, in the last 40%
            window = text[start + int(chunk_chars * 0.6):end]
            cut = max(window.rfind("\n\n"), window.rfind("\n"))
            if cut != -1:
                end = start + int(chunk_chars * 0.6) + cut
        yield idx, start, end, text[start:end]
        idx += 1
        if end >= content_end:
            break
        start = max(end - chunk_overlap, start + 1)


class EmbedStage(Stage):
    name = "embed"

    def run(self) -> StageStats:
        stats = StageStats()
        stats.inc("new_chunks", self._sync_chunks())
        stats.inc("payloads_updated", self._sync_payloads())

        store = ModelStore(self.config.models_dir)
        fingerprint = current_fingerprint(self.config, store)
        paths = self._resolve_index(fingerprint)
        paths.vecs_dir.mkdir(parents=True, exist_ok=True)
        thread_paths = thread_index_paths(self.config, fingerprint)
        thread_paths.vecs_dir.mkdir(parents=True, exist_ok=True)

        chunk_ids = {int(row["id"]) for row in self.conn.execute(
            "SELECT id FROM chunks")}
        have = {int(p.stem) for p in paths.vecs_dir.glob("*.npy")
                if p.stem.isdigit()}
        total = len(chunk_ids)
        thread_rows = self._thread_rows()
        expected_thread_files = {thread_vector_filename(
            row["thread_id"], row["summary_text"]) for row in thread_rows}
        existing_thread_files = {
            path.name for path in thread_paths.vecs_dir.glob("*.npy")}
        thread_pending = [row for row in thread_rows if not (
            thread_paths.vecs_dir /
            thread_vector_filename(row["thread_id"],
                                   row["summary_text"])).is_file()]
        chunks_current = chunk_ids.issubset(have) \
            and paths.vectors_npy.exists() \
            and paths.vectors_ids_npy.exists() \
            and set(np.load(paths.vectors_ids_npy).tolist()) == chunk_ids
        threads_current = not thread_pending and \
            thread_paths.vectors_npy.exists() and \
            thread_paths.vectors_ids_npy.exists() and \
            existing_thread_files == expected_thread_files and \
            set(np.load(thread_paths.vectors_ids_npy).tolist()) == {
                int(row["thread_id"]) for row in thread_rows}
        if chunks_current and threads_current:
            stats.inc("index_size", total)
            stats.inc("thread_index_size", len(thread_rows))
            return stats

        chunk_pending = bool(chunk_ids - have)
        backend = get_backend(self.config, store) \
            if chunk_pending or thread_pending else None
        chunk_done, chunk_failed = self._embed_pending(
            backend, paths.vecs_dir, have) if chunk_pending else (0, 0)
        thread_done, thread_failed = self._embed_pending_threads(
            backend, thread_paths, thread_pending) \
            if thread_pending else (0, 0)
        stats.inc("embedded", chunk_done + thread_done)
        stats.inc("embedded_chunks", chunk_done)
        stats.inc("embedded_threads", thread_done)
        stats.inc("failed", chunk_failed + thread_failed)
        perf = self.ctx.telemetry.embed
        assembly_started = time.monotonic()
        stats.inc("index_size",
                  self._rebuild_matrix(paths, fingerprint))
        stats.inc("thread_index_size",
                  self._rebuild_thread_matrix(thread_paths, fingerprint))
        perf.timings_seconds.matrix_assembly += \
            time.monotonic() - assembly_started
        return stats

    # -- chunking ------------------------------------------------------------

    def _sync_chunks(self) -> int:
        """Create chunk rows for any text artifact that has none yet.
        Chunks are immutable once created (source docs never change;
        changed source = custody alarm upstream)."""
        created = 0
        root = self.config.project_root
        chunk_args = (self.config.chunk_chars, self.config.chunk_overlap)

        emails = self.conn.execute(
            """SELECT id, body_text_path FROM emails
               WHERE body_text_path IS NOT NULL AND NOT EXISTS
                 (SELECT 1 FROM chunks c WHERE c.email_id = emails.id
                  AND c.source_type = 'email_body')""").fetchall()
        for row in emails:
            path = root / row["body_text_path"]
            text = message_body_text(path.read_bytes(), source=path)
            for idx, start, end, chunk in chunk_text(text, *chunk_args):
                self.conn.execute(
                    "INSERT INTO chunks (source_type, email_id, chunk_index,"
                    " text, char_start, char_end, translit_shadow)"
                    " VALUES ('email_body', ?, ?, ?, ?, ?, ?)",
                    (row["id"], idx, chunk, start, end,
                     proper_noun_shadow(chunk)))
                created += 1

        documents = self.conn.execute(
            """SELECT d.id, d.extracted_text_path
               FROM documents d
               WHERE d.extracted_text_path IS NOT NULL AND d.is_skipped = 0
                 AND d.extraction_method != 'error'
                 AND NOT EXISTS (SELECT 1 FROM chunks c
                                 WHERE c.document_id = d.id
                                   AND c.source_type = 'document_text')
               """).fetchall()
        for row in documents:
            text = (root / row["extracted_text_path"]).read_text(
                encoding="utf-8")
            for idx, start, end, chunk in chunk_text(text, *chunk_args):
                self.conn.execute(
                    "INSERT INTO chunks (source_type, document_id,"
                    " chunk_index, text, char_start, char_end,"
                    " translit_shadow)"
                    " VALUES ('document_text', ?, ?, ?, ?, ?, ?)",
                    (row["id"], idx, chunk, start, end,
                     proper_noun_shadow(chunk)))
                created += 1
        self.conn.commit()
        return created

    def _sync_payloads(self) -> int:
        """Converge the mutable FTS/embed shadow without re-chunking.

        A payload-recipe change selects a new vector directory through the
        fingerprint and this pass refreshes the FTS shadow over the same
        immutable chunk quotes.
        """
        rows = self.conn.execute(
            """SELECT chunks.id, chunks.text, chunks.source_type,
                      chunks.payload_shadow,
                      emails.date_utc, emails.date_raw, emails.from_name,
                      emails.from_addr, emails.to_addrs, emails.subject,
                      COALESCE(
                        (SELECT filename FROM attachments
                          WHERE document_id = documents.id
                          ORDER BY id LIMIT 1),
                        (SELECT relpath FROM document_sources
                          WHERE document_id = documents.id
                          ORDER BY id LIMIT 1)) AS document_name
                 FROM chunks
                 LEFT JOIN emails ON emails.id = chunks.email_id
                 LEFT JOIN documents ON documents.id = chunks.document_id
                ORDER BY chunks.id""").fetchall()
        updated = 0
        for row in rows:
            payload = enriched_payload(row)
            if row["payload_shadow"] == payload:
                continue
            self.conn.execute(
                "UPDATE chunks SET payload_shadow = ? WHERE id = ?",
                (payload, row["id"]))
            updated += 1
        self.conn.commit()
        return updated

    # -- vector cache ----------------------------------------------------------

    def _resolve_index(self, fingerprint: dict) -> IndexPaths:
        """Resolve the current model's cache directory. Never deletes
        another model's cache. Chunking config drift is reported but
        NOT auto-fixed — no automated re-chunk pipeline exists; existing
        chunks keep their original size, only new content uses the new
        config."""
        paths = index_paths(self.config, fingerprint)
        if paths.meta_json.exists():
            meta = json.loads(paths.meta_json.read_text())
            old = meta_fingerprint(meta)
            if chunking_fields_changed(old, fingerprint):
                print("embed: WARNING chunking config changed (chars"
                      f" {old['chunk_chars']}->{fingerprint['chunk_chars']},"
                      f" overlap {old['chunk_overlap']}->"
                      f"{fingerprint['chunk_overlap']}) but existing chunks"
                      " were NOT rebuilt — no automated re-chunk pipeline.")
            if old["chunk_chars"] != fingerprint["chunk_chars"] \
                    or old["chunk_overlap"] != fingerprint["chunk_overlap"]:
                meta["chunk_chars"] = fingerprint["chunk_chars"]
                meta["chunk_overlap"] = fingerprint["chunk_overlap"]
                _atomic_publish_json(paths.meta_json, meta)
        return paths

    def _embed_pending(self, backend, vecs_dir,
                       have: set[int]) -> tuple[int, int]:
        rows = self.conn.execute(
            "SELECT id, payload_shadow FROM chunks ORDER BY id").fetchall()
        pending = [r for r in rows if r["id"] not in have]
        jobs = [_PendingEmbedding(
            entity_id=int(row["id"]), text=row["payload_shadow"],
            target=vecs_dir / f"{row['id']}.npy",
            note=f"chunk {row['id']}", review_key=f"chunk:{row['id']}")
            for row in pending]
        return self._embed_queue(
            backend, jobs, self.ctx.telemetry.embed.queues.leaf,
            "embed text chunks")

    def _thread_rows(self):
        return self.conn.execute(
            """SELECT thread_id, summary_text FROM thread_summaries
               WHERE is_stale = 0 ORDER BY thread_id""").fetchall()

    def _embed_pending_threads(self, backend, paths: IndexPaths,
                               pending) -> tuple[int, int]:
        jobs = [_PendingEmbedding(
            entity_id=int(row["thread_id"]), text=row["summary_text"],
            target=paths.vecs_dir / thread_vector_filename(
                row["thread_id"], row["summary_text"]),
            note=f"thread {row['thread_id']}",
            review_key=f"thread:{row['thread_id']}") for row in pending]
        return self._embed_queue(
            backend, jobs, self.ctx.telemetry.embed.queues.summary,
            "embed thread summaries")

    @staticmethod
    def _bucket_for(token_count: int) -> int:
        if token_count < 1:
            token_count = 1
        if token_count > EMBED_MAX_TOKENS:
            raise ValueError(
                f"embedding input exceeds {EMBED_MAX_TOKENS} tokens")
        return min(
            EMBED_MAX_TOKENS,
            ((token_count + EMBED_BUCKET_WIDTH - 1) //
             EMBED_BUCKET_WIDTH) * EMBED_BUCKET_WIDTH)

    def _embed_queue(self, backend, jobs: list[_PendingEmbedding], queue,
                     progress_label: str) -> tuple[int, int]:
        """Embed one namespace with deterministic buckets and failure splits."""
        perf = self.ctx.telemetry.embed
        queue.pending_entities = len(jobs)
        progress = Progress(progress_label, total=len(jobs))
        buckets: dict[int, list[tuple[_PendingEmbedding, int]]] = {}
        outcomes: list[tuple[_PendingEmbedding, Exception | None]] = []

        for job in jobs:
            try:
                token_count = backend.count_tokens(job.text)
                queue.input_tokens += token_count
                bucket = self._bucket_for(token_count)
                buckets.setdefault(bucket, []).append((job, token_count))
            except Exception as exc:
                outcomes.append((job, exc))
        queue.bucket_count = len(buckets)

        def model_many(batch, bucket: int):
            queue.microbatch_count += 1
            queue.padding_tokens += sum(
                bucket - token_count for _, token_count in batch)
            started = time.monotonic()
            try:
                result = backend.embed_many(
                    [job.text for job, _ in batch],
                    pad_to_tokens=bucket)
            finally:
                perf.timings_seconds.model_execution += \
                    time.monotonic() - started
            if len(result) != len(batch):
                raise ValueError(
                    f"batch returned {len(result)} vectors for"
                    f" {len(batch)} entities")
            return result

        def model_one(job: _PendingEmbedding):
            queue.individual_fallbacks += 1
            started = time.monotonic()
            try:
                return backend.embed_one(job.text)
            finally:
                perf.timings_seconds.model_execution += \
                    time.monotonic() - started

        def publish(job: _PendingEmbedding, vector) -> Exception | None:
            try:
                checked = _validated_vector(vector, backend.dim)
            except Exception:
                try:
                    checked = _validated_vector(model_one(job), backend.dim)
                except Exception as exc:
                    return exc
            started = time.monotonic()
            try:
                _atomic_publish_array(job.target, checked)
            except Exception as exc:
                return exc
            finally:
                perf.timings_seconds.cache_publication += \
                    time.monotonic() - started
            perf.verified_cache_publications += 1
            return None

        def execute(batch, bucket: int) -> None:
            try:
                vectors = model_many(batch, bucket)
            except Exception as batch_exc:
                if len(batch) > 1:
                    queue.bisection_fallbacks += 1
                    middle = len(batch) // 2
                    execute(batch[:middle], bucket)
                    execute(batch[middle:], bucket)
                    return
                job, _ = batch[0]
                try:
                    vector = model_one(job)
                except Exception as exc:
                    outcomes.append((job, exc))
                    return
                outcomes.append((job, publish(job, vector)))
                return
            for (job, _), vector in zip(batch, vectors, strict=True):
                outcomes.append((job, publish(job, vector)))

        for bucket in sorted(buckets):
            bucket_jobs = buckets[bucket]
            for start in range(0, len(bucket_jobs), EMBED_BATCH_SIZE):
                batch = bucket_jobs[start:start + EMBED_BATCH_SIZE]
                progress.start(note=batch[0][0].note)
                execute(batch, bucket)

        done = failed = 0
        for job, error in outcomes:
            progress.step(note=job.note)
            if error is None:
                done += 1
                queue.successful_entities += 1
                continue
            failed += 1
            queue.failed_entities += 1
            progress.println(
                f"  embed FAIL {job.note}: {type(error).__name__}: {error}")
            self.review.flag(
                job.review_key, self.name, "error",
                f"{type(error).__name__}: {error}")
        progress.done()
        self.conn.commit()
        return done, failed

    def _rebuild_matrix(self, paths: IndexPaths, fingerprint: dict) -> int:
        """Rebuild the matrix from the per-chunk cache directory — the
        cache dir is the source of truth, not the previous matrix."""
        chunk_ids = [r["id"] for r in self.conn.execute(
            "SELECT id FROM chunks ORDER BY id")]
        vecs, ids = [], []
        for cid in chunk_ids:
            path = paths.vecs_dir / f"{cid}.npy"
            if not path.is_file():
                continue  # not embedded yet (or failed) — retried next run
            vecs.append(_validated_vector(
                np.load(path, allow_pickle=False), fingerprint["dim"]))
            ids.append(cid)

        paths.vectors_npy.parent.mkdir(parents=True, exist_ok=True)
        matrix = np.stack(vecs) if vecs else \
            np.zeros((0, fingerprint["dim"]), dtype=np.float32)
        _atomic_publish_array(paths.vectors_npy, matrix)
        _atomic_publish_array(
            paths.vectors_ids_npy, np.asarray(ids, dtype=np.int64))
        _atomic_publish_json(paths.meta_json, {
            **fingerprint,
            "count": len(ids),
            "built_at": datetime.now(timezone.utc).isoformat(),
        })
        return len(ids)

    def _rebuild_thread_matrix(self, paths: IndexPaths,
                               fingerprint: dict) -> int:
        rows = self._thread_rows()
        expected: set[str] = set()
        vecs, ids = [], []
        for row in rows:
            filename = thread_vector_filename(
                row["thread_id"], row["summary_text"])
            expected.add(filename)
            path = paths.vecs_dir / filename
            if not path.is_file():
                continue
            vecs.append(_validated_vector(
                np.load(path, allow_pickle=False), fingerprint["dim"]))
            ids.append(row["thread_id"])

        matrix = np.stack(vecs) if vecs else \
            np.zeros((0, fingerprint["dim"]), dtype=np.float32)
        _atomic_publish_array(paths.vectors_npy, matrix)
        _atomic_publish_array(
            paths.vectors_ids_npy, np.asarray(ids, dtype=np.int64))
        _atomic_publish_json(paths.meta_json, {
            **fingerprint,
            "kind": "thread_summaries",
            "count": len(ids),
            "built_at": datetime.now(timezone.utc).isoformat(),
        })
        for path in paths.vecs_dir.glob("*.npy"):
            if path.name not in expected:
                path.unlink()
        return len(ids)
