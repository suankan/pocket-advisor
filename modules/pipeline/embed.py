"""Stage 6 — embedding convergence.

Producers dispatch embedding payloads at artifact readiness
(`docs/features/embedding-design-v2.md`, decision 5); this stage is the
authoritative convergence pass: it sweeps for chunk rows any producer run
missed, converges payload shadows, backfills every pending vector through
the inference endpoint (failures, downtime, interrupted runs), reports
loudly, and rebuilds both aligned matrices from the durable per-entity
cache.

Incremental: an entity is pending for the CURRENT fingerprint whenever it
has no published vector file yet — the per-entity cache is the durable
source of truth, so a crash mid-run loses at most the in-flight requests,
and a failed entity is retried next run.
"""
import json
import os
import tempfile
import time
from datetime import datetime, timezone
from pathlib import Path

import numpy as np

from modules.integrity import write_verified
from modules.domain import StageStats
from modules.embedding import (IndexPaths, atomic_publish_array,
                               chunking_fields_changed, current_fingerprint,
                               get_backend, index_paths, meta_fingerprint,
                               thread_index_paths, thread_vector_filename,
                               validated_vector)
from modules.embedding.chunks import (chunk_text, sync_document_chunks,
                                      sync_email_chunks, sync_payloads)
from modules.embedding.dispatch import EmbedDispatcher
from modules.pipeline.base import Stage
from modules.progress import Progress

__all__ = ["EmbedStage", "chunk_text"]


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


class EmbedStage(Stage):
    name = "embed"

    def run(self) -> StageStats:
        stats = StageStats()
        created = sync_email_chunks(self.conn, self.config)
        created += sync_document_chunks(self.conn, self.config)
        stats.inc("new_chunks", created)
        stats.inc("payloads_updated", sync_payloads(self.conn))
        self.conn.commit()
        self._settle_readiness_dispatch()

        fingerprint = current_fingerprint(self.config)
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
        if chunk_pending or thread_pending:
            self._converge_pending(stats, fingerprint, thread_pending)

        perf = self.ctx.telemetry.embed
        assembly_started = time.monotonic()
        stats.inc("index_size",
                  self._rebuild_matrix(paths, fingerprint))
        stats.inc("thread_index_size",
                  self._rebuild_thread_matrix(thread_paths, fingerprint))
        perf.timings_seconds.matrix_assembly += \
            time.monotonic() - assembly_started
        return stats

    # -- convergence backfill ------------------------------------------------

    def _settle_readiness_dispatch(self) -> None:
        """Drain the run-wide readiness dispatcher first so the per-entity
        cache reflects every in-flight publication; whatever failed or was
        skipped simply remains a gap that the loud backfill below owns."""
        dispatcher = self.ctx.embed_dispatcher
        if dispatcher is None:
            return
        pending = dispatcher.pending_count
        if pending:
            print(f"embed: waiting for {pending} in-flight readiness"
                  " dispatches…")
        dispatcher.drain()
        dispatcher.close()
        self.ctx.embed_dispatcher = None

    def _converge_pending(self, stats: StageStats, fingerprint: dict,
                          thread_pending) -> None:
        """Backfill every gap through the inference endpoint — the loud,
        authoritative pass: readiness dispatch may silently degrade, this
        may not."""
        backend = get_backend(self.config)
        check_ready = getattr(backend, "check_ready", None)
        if check_ready is not None:
            check_ready()
        dispatcher = EmbedDispatcher(
            self.ctx, backend=backend, fingerprint=fingerprint)
        submitted = dispatcher.submit_pending_leaves(self.conn)
        for row in thread_pending:
            if dispatcher.submit_summary(
                    int(row["thread_id"]), row["summary_text"]):
                submitted += 1
        progress = Progress("embed pending entities", total=submitted)
        try:
            done, failed, skipped, outcomes = dispatcher.drain(progress)
        finally:
            progress.done()
            dispatcher.close()

        chunk_done = sum(1 for item in outcomes
                         if item.error is None and not item.skipped
                         and item.review_key.startswith("chunk:"))
        stats.inc("embedded", done)
        stats.inc("embedded_chunks", chunk_done)
        stats.inc("embedded_threads", done - chunk_done)
        stats.inc("failed", failed)
        for outcome in outcomes:
            if outcome.error is None:
                continue
            print(f"  embed FAIL {outcome.note}: {outcome.error}")
            self.review.flag(
                outcome.review_key, self.name, "error", outcome.error)
        self.conn.commit()
        if skipped and dispatcher.unavailable is not None:
            raise SystemExit(
                f"embed: {dispatcher.unavailable} — {skipped} entities left"
                " pending; rerun 'ingest embed' once oMLX is up")

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

    def _thread_rows(self):
        return self.conn.execute(
            """SELECT thread_id, summary_text FROM thread_summaries
               WHERE is_stale = 0 ORDER BY thread_id""").fetchall()

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
            vecs.append(validated_vector(
                np.load(path, allow_pickle=False), fingerprint["dim"]))
            ids.append(cid)

        paths.vectors_npy.parent.mkdir(parents=True, exist_ok=True)
        matrix = np.stack(vecs) if vecs else \
            np.zeros((0, fingerprint["dim"]), dtype=np.float32)
        atomic_publish_array(paths.vectors_npy, matrix)
        atomic_publish_array(
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
            vecs.append(validated_vector(
                np.load(path, allow_pickle=False), fingerprint["dim"]))
            ids.append(row["thread_id"])

        matrix = np.stack(vecs) if vecs else \
            np.zeros((0, fingerprint["dim"]), dtype=np.float32)
        atomic_publish_array(paths.vectors_npy, matrix)
        atomic_publish_array(
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
