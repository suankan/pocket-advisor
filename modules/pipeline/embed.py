"""Stage 4 — chunk the plain-text artifacts and embed via Jina MLX.

Inputs are exactly the Stage 2/3 text artifacts, read through the DB:
- items.body_text_path — authored email messages AND native-PDF texts
  (chunk source_type 'email_body', name kept for retrieval compat)
- attachments.extracted_text_path — attachment pdf-to-text artifacts
  (chunk source_type 'attachment')

Chunks ~chunk_chars with ~chunk_overlap, splitting on paragraph
boundaries where possible; every artifact yields >=1 chunk so every
item is individually citable. Chunks are immutable once created.

Incremental: a chunk is pending for the CURRENT model whenever its id
has no <vecs_dir>/<id>.npy yet — the per-chunk cache is the durable
source of truth, so a crash mid-run loses at most one chunk, and a
failed chunk is retried next run. Each run rebuilds vectors.npy +
vectors_ids.npy from that cache.
"""
import json
from collections.abc import Iterator
from datetime import datetime, timezone

import numpy as np

from modules.domain import StageStats
from modules.emailbody import body_text as message_body_text
from modules.embedding import (IndexPaths, ModelStore,
                               chunking_fields_changed, current_fingerprint,
                               enriched_payload, get_backend, index_paths,
                               meta_fingerprint, thread_index_paths,
                               thread_vector_filename)
from modules.pipeline.base import Stage
from modules.progress import Progress
from modules.transliteration import proper_noun_shadow


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
        stats.inc("index_size",
                  self._rebuild_matrix(paths, fingerprint))
        stats.inc("thread_index_size",
                  self._rebuild_thread_matrix(thread_paths, fingerprint))
        return stats

    # -- chunking ------------------------------------------------------------

    def _sync_chunks(self) -> int:
        """Create chunk rows for any text artifact that has none yet.
        Chunks are immutable once created (source docs never change;
        changed source = custody alarm upstream)."""
        created = 0
        root = self.config.project_root
        chunk_args = (self.config.chunk_chars, self.config.chunk_overlap)

        items = self.conn.execute(
            """SELECT id, item_kind, body_text_path FROM items
               WHERE body_text_path IS NOT NULL AND NOT EXISTS
                 (SELECT 1 FROM chunks c WHERE c.item_id = items.id
                  AND c.source_type = 'email_body')""").fetchall()
        for row in items:
            path = root / row["body_text_path"]
            text = message_body_text(path.read_bytes(), source=path) \
                if row["item_kind"] == "email" else path.read_text(
                    encoding="utf-8")
            for idx, start, end, chunk in chunk_text(text, *chunk_args):
                self.conn.execute(
                    "INSERT INTO chunks (source_type, item_id, chunk_index,"
                    " text, char_start, char_end, translit_shadow)"
                    " VALUES ('email_body', ?, ?, ?, ?, ?, ?)",
                    (row["id"], idx, chunk, start, end,
                     proper_noun_shadow(chunk)))
                created += 1

        attachments = self.conn.execute(
            """SELECT a.id, a.item_id, a.extracted_text_path
               FROM attachments a
               WHERE a.extracted_text_path IS NOT NULL AND a.is_skipped = 0
                 AND a.extraction_method != 'error'
                 AND NOT EXISTS (SELECT 1 FROM chunks c
                                 WHERE c.attachment_id = a.id)""").fetchall()
        for row in attachments:
            text = (root / row["extracted_text_path"]).read_text(
                encoding="utf-8")
            for idx, start, end, chunk in chunk_text(text, *chunk_args):
                self.conn.execute(
                    "INSERT INTO chunks (source_type, item_id,"
                    " attachment_id, chunk_index, text, char_start,"
                    " char_end, translit_shadow)"
                    " VALUES ('attachment', ?, ?, ?, ?, ?, ?, ?)",
                    (row["item_id"], row["id"], idx, chunk, start, end,
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
                      chunks.payload_shadow, items.item_kind,
                      items.date_utc, items.date_raw, items.from_name,
                      items.from_addr, items.to_addrs, items.subject,
                      attachments.filename AS attachment_name,
                      COALESCE(
                        (SELECT item_memberships.filename
                           FROM item_memberships
                          WHERE item_memberships.item_id = items.id
                          ORDER BY item_memberships.id LIMIT 1),
                        items.subject) AS document_name
                 FROM chunks
                 JOIN items ON items.id = chunks.item_id
                 LEFT JOIN attachments
                   ON attachments.id = chunks.attachment_id
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
                paths.meta_json.write_text(json.dumps(meta, indent=2))
        return paths

    def _embed_pending(self, backend, vecs_dir,
                       have: set[int]) -> tuple[int, int]:
        rows = self.conn.execute(
            "SELECT id, payload_shadow FROM chunks").fetchall()
        pending = [r for r in rows if r["id"] not in have]
        done, failed = 0, 0
        progress = Progress("embed text chunks", total=len(pending))
        for row in pending:
            progress.step(note=f"chunk {row['id']}")
            try:
                vec = backend.embed_one(row["payload_shadow"])
                np.save(vecs_dir / f"{row['id']}.npy", vec)
                done += 1
            except Exception as exc:
                failed += 1
                progress.println(f"  embed FAIL chunk {row['id']}:"
                                 f" {type(exc).__name__}: {exc}")
                self.review.flag(f"chunk:{row['id']}", self.name, "error",
                                 f"{type(exc).__name__}: {exc}")
        progress.done()
        self.conn.commit()
        return done, failed

    def _thread_rows(self):
        return self.conn.execute(
            """SELECT thread_id, summary_text FROM thread_summaries
               WHERE is_stale = 0 ORDER BY thread_id""").fetchall()

    def _embed_pending_threads(self, backend, paths: IndexPaths,
                               pending) -> tuple[int, int]:
        done, failed = 0, 0
        progress = Progress("embed thread summaries", total=len(pending))
        for row in pending:
            progress.step(note=f"thread {row['thread_id']}")
            target = paths.vecs_dir / thread_vector_filename(
                row["thread_id"], row["summary_text"])
            try:
                vec = backend.embed_one(row["summary_text"])
                np.save(target, vec)
                done += 1
            except Exception as exc:
                failed += 1
                progress.println(
                    f"  embed FAIL thread {row['thread_id']}:"
                    f" {type(exc).__name__}: {exc}")
                self.review.flag(
                    f"thread:{row['thread_id']}", self.name, "error",
                    f"{type(exc).__name__}: {exc}")
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
            vecs.append(np.load(path))
            ids.append(cid)

        paths.vectors_npy.parent.mkdir(parents=True, exist_ok=True)
        matrix = np.stack(vecs) if vecs else \
            np.zeros((0, fingerprint["dim"]), dtype=np.float32)
        np.save(paths.vectors_npy, matrix)
        np.save(paths.vectors_ids_npy, np.asarray(ids, dtype=np.int64))
        paths.meta_json.write_text(json.dumps({
            **fingerprint,
            "count": len(ids),
            "built_at": datetime.now(timezone.utc).isoformat(),
        }, indent=2))
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
            vecs.append(np.load(path))
            ids.append(row["thread_id"])

        for path in paths.vecs_dir.glob("*.npy"):
            if path.name not in expected:
                path.unlink()

        matrix = np.stack(vecs) if vecs else \
            np.zeros((0, fingerprint["dim"]), dtype=np.float32)
        np.save(paths.vectors_npy, matrix)
        np.save(paths.vectors_ids_npy, np.asarray(ids, dtype=np.int64))
        paths.meta_json.write_text(json.dumps({
            **fingerprint,
            "kind": "thread_summaries",
            "count": len(ids),
            "built_at": datetime.now(timezone.utc).isoformat(),
        }, indent=2))
        return len(ids)
