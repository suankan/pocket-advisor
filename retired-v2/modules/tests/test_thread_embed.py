"""Self-test: ThreadStage + EmbedStage (fake MLX backend, no model)."""
import json
import sys
import tempfile
from pathlib import Path
from unittest.mock import patch

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

import v2.modules.pipeline.embed as embed_mod  # noqa: E402
from v2.modules.config import Config  # noqa: E402
from v2.modules.integrity import sha256_bytes  # noqa: E402
from v2.modules.database import Database  # noqa: E402
from v2.modules.emailbody import body_text  # noqa: E402
from v2.modules.embedding import (EMBED_EXECUTION_RECIPE, PAYLOAD_RECIPE,  # noqa: E402
                               index_paths)
from v2.modules.chunk_reader import ChunkReader  # noqa: E402
from v2.modules.embedding.chunks import chunk_payload  # noqa: E402
from v2.modules.pipeline.base import PipelineContext  # noqa: E402
from v2.modules.pipeline.embed import EmbedStage  # noqa: E402
from v2.modules.pipeline.thread import ThreadStage  # noqa: E402
from v2.modules.review import ReviewLog  # noqa: E402
from v2.modules.telemetry import PerformanceTelemetry  # noqa: E402
from v2.modules.workspace import Registry  # noqa: E402

REGISTRY_YAML = """\
schema_version: 2
collections:
  - id: mail
    path: corpora/mail
workspaces:
  - id: matter-x
    collections:
      - id: mail
"""

DIM = 4
FINGERPRINT = {"backend": "mlx", "model": "fake/model", "dim": DIM,
               "chunk_chars": 1500, "chunk_overlap": 200,
               "payload_recipe": PAYLOAD_RECIPE,
               "execution_recipe": EMBED_EXECUTION_RECIPE}


class FakeBackend:
    def __init__(self, fail_texts: set[str] = frozenset()):
        self.dim = DIM
        self.fail_texts = fail_texts
        self.embedded_texts: list[str] = []

    def embed_one(self, text: str, is_query: bool = False):
        if any(blocked in text for blocked in self.fail_texts):
            raise RuntimeError("simulated embed failure")
        self.embedded_texts.append(text)
        seed = sum(text.encode()) % 97 + 1
        vec = np.arange(1, DIM + 1, dtype=np.float32) * seed
        return vec / np.linalg.norm(vec)

    def embed_many(self, texts: list[str], *, pad_to_tokens: int):
        return [self.embed_one(text) for text in texts]

    def count_tokens(self, text: str, is_query: bool = False) -> int:
        return len(text.split()) + 2


def insert_item(conn, tmp: Path, mid: str, subject: str, body: str,
                date_utc: str, from_addr: str, to_addr: str,
                in_reply_to: str | None = None,
                references: str | None = None) -> int:
    """Insert one synthetic `emails` + `email_sources` row pair mirroring
    Stage 2's column list (`modules/pipeline/emails.py`). Identity is now
    `emails.sha256` (the real UNIQUE key — `message_id` is a plain,
    non-unique column post-cutover), synthesized deterministically from
    this call's content so distinct calls never collide.
    """
    body_path = tmp / "messages" / \
        f"{mid.strip('<>').replace('@', '_')}" / "email_message.txt"
    body_path.parent.mkdir(parents=True, exist_ok=True)
    body_path.write_text(
        f"Date: {date_utc}\nFrom: {from_addr}\nTo: {to_addr}\nCc: "
        f"\nSubject: {subject}\n\n{body}", encoding="utf-8")
    sha = sha256_bytes("\x1f".join((
        mid, subject, body, date_utc, from_addr, to_addr,
        in_reply_to or "", references or "")).encode("utf-8"))
    to_json = json.dumps([{"name": None, "addr": to_addr}])
    cur = conn.execute(
        """INSERT INTO emails (sha256, message_id, subject,
           subject_normalized, date_utc, from_addr, to_addrs, cc_addrs,
           in_reply_to, references_raw, body_text_path, ingested_at)
           VALUES (?, ?, ?, ?, ?, ?, ?, '[]', ?, ?, ?, 't')""",
        (sha, mid, subject, subject.removeprefix("Re: ").lower(), date_utc,
         from_addr, to_json, in_reply_to, references,
         str(body_path.relative_to(tmp))))
    email_id = int(cur.lastrowid)
    conn.execute(
        """INSERT INTO email_sources
           (email_id, workspace_id, collection_id, relpath,
            file_size_bytes, discovered_at)
           VALUES (?, 'matter-x', 'mail', ?, ?, 't')""",
        (email_id, f"{mid}.eml", len(body.encode("utf-8"))))
    return email_id


def main() -> int:
    with tempfile.TemporaryDirectory(prefix="pa_thread_embed_") as td:
        tmp = Path(td)
        ws_dir = tmp / "workspaces"
        ws_dir.mkdir(parents=True)
        (ws_dir / "workspace-config.yaml").write_text(REGISTRY_YAML)
        base = Config(project_root=tmp, workspaces_dir=ws_dir)
        registry = Registry.load(base)
        workspace = registry.require_workspace("matter-x")
        cfg = base.for_workspace(workspace.id)
        conn = Database(cfg.db_path, workspace.id).open()
        ctx = PipelineContext(
            config=cfg, registry=registry, workspace=workspace, conn=conn,
            review=ReviewLog(conn, cfg.review_queue_csv))

        a = insert_item(conn, tmp, "<a@x>", "Settlement", "Original text.",
                        "2024-01-01T10:00:00+00:00", "alice@x", "bob@y")
        b = insert_item(conn, tmp, "<b@x>", "Re: Settlement",
                        "Reply text про Ксению и договор.",
                        "2024-01-02T10:00:00+00:00", "bob@y", "alice@x",
                        in_reply_to="<a@x>", references="<a@x>")
        c = insert_item(conn, tmp, "<c@x>", "Settlement",
                        "Headerless client copy.",
                        "2024-01-10T10:00:00+00:00", "alice@x", "carol@z")
        d = insert_item(conn, tmp, "<d@x>", "Invoice", "Unrelated.",
                        "2024-03-01T10:00:00+00:00", "dave@w", "erin@v")
        conn.commit()

        # -- threading -----------------------------------------------------
        tstats = ThreadStage(ctx).run()
        assert tstats.get("method_reference") == 2, tstats
        assert tstats.get("method_subject_heuristic") == 1, tstats
        assert tstats.get("method_singleton") == 1, tstats
        assert tstats.get("threads") == 2, tstats
        threads = {row["message_id"]: row["thread_id"] for row in
                   conn.execute("SELECT message_id, thread_id FROM emails")}
        assert threads["<a@x>"] == threads["<b@x>"] == threads["<c@x>"]
        assert threads["<d@x>"] != threads["<a@x>"]
        rep = conn.execute(
            "SELECT representative_subject, item_count FROM threads"
            " WHERE id = ?", (threads["<a@x>"],)).fetchone()
        assert rep["item_count"] == 3 and \
            rep["representative_subject"] == "Settlement"

        # An email-carried document with extracted text + a skipped one
        # (never chunked) — `documents` is 1:1 with content identity, so an
        # `attachments` occurrence row links each to the carrying email.
        att_txt = tmp / "att" / "1.txt"
        att_txt.parent.mkdir(parents=True)
        att_txt.write_text("Statement line items text.", encoding="utf-8")
        doc1 = conn.execute(
            """INSERT INTO documents (sha256, media_kind, size_bytes,
                      extraction_method, extracted_text_path, is_skipped,
                      ingested_at)
               VALUES ('doc-sha-x', 'pdf', 123,
                       'ocrmypdf_redo_clean_pdftotext_layout', ?, 0, 't')""",
            (str(att_txt.relative_to(tmp)),)).lastrowid
        conn.execute(
            """INSERT INTO attachments
                      (email_id, document_id, filename, ordinal, ingested_at)
               VALUES (?, ?, 's.pdf', 0, 't')""", (a, doc1))
        doc2 = conn.execute(
            """INSERT INTO documents (sha256, media_kind, size_bytes,
                      extraction_method, is_skipped, skip_reason,
                      ingested_at)
               VALUES ('doc-sha-y', 'image', 456, 'stored_only', 1,
                       'image', 't')""").lastrowid
        conn.execute(
            """INSERT INTO attachments
                      (email_id, document_id, filename, ordinal, ingested_at)
               VALUES (?, ?, 'logo.png', 1, 't')""", (a, doc2))

        # A native document (mounted directly, not carried by any email) is
        # chunked via source_type 'document_text' and receives a
        # "Document: <name>" payload prefix (modules/embedding/payloads.py).
        doc_path = tmp / "docs" / "notice.txt"
        doc_path.parent.mkdir(parents=True)
        doc_path.write_text("Native filing content.", encoding="utf-8")
        doc3 = conn.execute(
            """INSERT INTO documents (sha256, media_kind, size_bytes,
                      extraction_method, extracted_text_path, is_skipped,
                      ingested_at)
               VALUES ('native-sha', 'pdf', 789, 'pdftotext_layout', ?, 0,
                       't')""",
            (str(doc_path.relative_to(tmp)),)).lastrowid
        conn.execute(
            """INSERT INTO document_sources
                      (document_id, workspace_id, collection_id, relpath,
                       file_size_bytes, discovered_at)
               VALUES (?, 'matter-x', 'mail', 'notice.pdf', 789, 't')""",
            (doc3,))
        conn.commit()

        # -- embedding -------------------------------------------------------
        backend = FakeBackend()
        with patch.object(embed_mod, "current_fingerprint",
                          return_value=dict(FINGERPRINT)), \
             patch.object(embed_mod, "get_backend",
                          return_value=backend):
            estats = EmbedStage(ctx).run()
        assert estats.get("new_chunks") == 6, estats
        # Producers feed chunks_fts directly at chunk creation; the
        # convergence-pass refeed only fires on a payload-recipe change or
        # a count mismatch, neither true here.
        assert estats.get("payloads_updated") == 0, estats
        assert estats.get("embedded") == 6, estats
        assert estats.get("failed") == 0, estats
        assert estats.get("index_size") == 6, estats

        # Chunks are offset-only rows; text/payload are re-derived through
        # the chunk reader, never read back from storage (chunks_fts is
        # contentless — it matches, it does not return column values).
        reader = ChunkReader(conn, cfg)
        rows = conn.execute(
            """SELECT chunks.id, chunks.source_type, chunks.email_id,
                      chunks.document_id, chunks.char_start,
                      chunks.char_end, emails.message_id, emails.date_utc,
                      emails.date_raw, emails.from_name, emails.from_addr,
                      emails.to_addrs, emails.subject,
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
        payloads = [chunk_payload(row, reader.chunk_text(row))
                    for row in rows]
        reply_row = next(row for row in rows
                         if row["message_id"] == "<b@x>")
        reply_text = reader.chunk_text(reply_row)
        assert reply_text == "Reply text про Ксению и договор."
        reply_payload = chunk_payload(reply_row, reply_text)
        assert reply_payload.startswith(
            "From: bob@y\nDate: 2024-01-02T10:00:00+00:00\n"
            "Subject: Re: Settlement\nTo: alice@x\n\n")
        message_path = tmp / conn.execute(
            "SELECT body_text_path FROM emails WHERE id = ?",
            (b,)).fetchone()[0]
        authored = body_text(message_path.read_bytes(), source=message_path)
        assert authored[reply_row["char_start"]:
                        reply_row["char_end"]] == reply_text

        attachment_row, attachment_payload = next(
            (row, payload) for row, payload in zip(rows, payloads)
            if row["source_type"] == "document_text"
            and payload.startswith("Document: s.pdf"))
        assert attachment_payload == \
            "Document: s.pdf\n\nStatement line items text."
        document_row, document_payload = next(
            (row, payload) for row, payload in zip(rows, payloads)
            if row["source_type"] == "document_text"
            and payload.startswith("Document: notice.pdf"))
        assert document_payload == \
            "Document: notice.pdf\n\nNative filing content."
        assert set(backend.embedded_texts) == set(payloads)

        fts_envelope = conn.execute(
            "SELECT COUNT(*) FROM chunks_fts WHERE chunks_fts"
            " MATCH 'Settlement'").fetchone()[0]
        assert fts_envelope >= 3

        # translit_shadow (proper-noun fallback) is only ever searchable,
        # never re-read — confirm via a transliterated MATCH instead of a
        # column select.
        translit_hits = conn.execute(
            "SELECT chunks_fts.rowid FROM chunks_fts"
            " JOIN chunks ON chunks.id = chunks_fts.rowid"
            " WHERE chunks_fts MATCH 'translit_shadow:Kseni*'"
            " AND chunks.email_id = ?"
            " AND chunks.source_type = 'email_body'", (b,)).fetchall()
        assert translit_hits, "expected proper-noun shadow to be searchable"

        paths = index_paths(cfg, FINGERPRINT)
        matrix = np.load(paths.vectors_npy)
        ids = np.load(paths.vectors_ids_npy)
        assert matrix.shape == (6, DIM) and len(ids) == 6
        meta = json.loads(paths.meta_json.read_text())
        assert meta["count"] == 6 and meta["model"] == "fake/model"

        # Re-run: up to date, backend never loaded.
        def boom(*_a, **_k):
            raise AssertionError("backend must not load when up to date")
        with patch.object(embed_mod, "current_fingerprint",
                          return_value=dict(FINGERPRINT)), \
             patch.object(embed_mod, "get_backend", side_effect=boom):
            estats2 = EmbedStage(ctx).run()
        assert estats2.get("index_size") == 6, estats2
        assert estats2.get("payloads_updated") == 0, estats2
        assert estats2.get("embedded", ) == 0, estats2

        # One bad member bisects its batch without losing the successful peer;
        # the missing entity alone is retried on the next run.
        e = insert_item(conn, tmp, "<e@x>", "Late arrival", "New text.",
                        "2024-04-01T10:00:00+00:00", "alice@x", "bob@y")
        f = insert_item(conn, tmp, "<f@x>", "Late peer", "Peer text.",
                        "2024-04-02T10:00:00+00:00", "alice@x", "bob@y")
        conn.commit()
        ctx.telemetry = PerformanceTelemetry()
        with patch.object(embed_mod, "current_fingerprint",
                          return_value=dict(FINGERPRINT)), \
             patch.object(embed_mod, "get_backend",
                          return_value=FakeBackend(
                              fail_texts={"New text."})):
            estats3 = EmbedStage(ctx).run()
        assert estats3.get("failed") == 1 and \
            estats3.get("embedded") == 1 and \
            estats3.get("index_size") == 7, estats3
        queue = ctx.telemetry.embed.queues.leaf
        assert queue.successful_entities == 1 and \
            queue.failed_entities == 1, queue
        assert ctx.telemetry.embed.verified_cache_publications == 1
        e_chunk = conn.execute(
            "SELECT id FROM chunks WHERE email_id = ?", (e,)).fetchone()[0]
        f_chunk = conn.execute(
            "SELECT id FROM chunks WHERE email_id = ?", (f,)).fetchone()[0]
        assert not (paths.vecs_dir / f"{e_chunk}.npy").exists()
        assert (paths.vecs_dir / f"{f_chunk}.npy").is_file()
        assert not any(path.name.endswith(".tmp")
                       for path in paths.vecs_dir.iterdir())
        ctx.telemetry = PerformanceTelemetry()
        with patch.object(embed_mod, "current_fingerprint",
                          return_value=dict(FINGERPRINT)), \
             patch.object(embed_mod, "get_backend",
                          return_value=FakeBackend()):
            estats4 = EmbedStage(ctx).run()
        assert estats4.get("embedded") == 1 and \
            estats4.get("index_size") == 8, estats4

        conn.close()
    print("test_thread_embed: all ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
