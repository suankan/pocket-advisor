"""Self-test: ThreadStage + EmbedStage (fake MLX backend, no model)."""
import json
import sys
import tempfile
from pathlib import Path
from unittest.mock import patch

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

import modules.pipeline.embed as embed_mod  # noqa: E402
from modules.config import Config  # noqa: E402
from modules.database import Database  # noqa: E402
from modules.emailbody import body_text  # noqa: E402
from modules.embedding import PAYLOAD_RECIPE, index_paths  # noqa: E402
from modules.pipeline.base import PipelineContext  # noqa: E402
from modules.pipeline.embed import EmbedStage  # noqa: E402
from modules.pipeline.thread import ThreadStage  # noqa: E402
from modules.review import ReviewLog  # noqa: E402
from modules.workspace import Registry  # noqa: E402

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
               "payload_recipe": PAYLOAD_RECIPE}


class FakeBackend:
    def __init__(self, fail_texts: set[str] = frozenset()):
        self.fail_texts = fail_texts
        self.embedded_texts: list[str] = []

    def embed_one(self, text: str, is_query: bool = False):
        if any(blocked in text for blocked in self.fail_texts):
            raise RuntimeError("simulated embed failure")
        self.embedded_texts.append(text)
        seed = sum(text.encode()) % 97 + 1
        vec = np.arange(1, DIM + 1, dtype=np.float32) * seed
        return vec / np.linalg.norm(vec)


def insert_item(conn, tmp: Path, mid: str, subject: str, body: str,
                date_utc: str, from_addr: str, to_addr: str,
                in_reply_to: str | None = None,
                references: str | None = None) -> int:
    body_path = tmp / "messages" / \
        f"{mid.strip('<>').replace('@', '_')}" / "email_message.txt"
    body_path.parent.mkdir(parents=True, exist_ok=True)
    body_path.write_text(
        f"Date: {date_utc}\nFrom: {from_addr}\nTo: {to_addr}\nCc: "
        f"\nSubject: {subject}\n\n{body}", encoding="utf-8")
    to_json = json.dumps([{"name": None, "addr": to_addr}])
    cur = conn.execute(
        """INSERT INTO items (item_kind, message_id, subject,
           subject_normalized, date_utc, from_addr, to_addrs, cc_addrs,
           in_reply_to, references_raw, body_text_path, ingested_at)
           VALUES ('email', ?, ?, ?, ?, ?, ?, '[]', ?, ?, ?, 't')""",
        (mid, subject, subject.removeprefix("Re: ").lower(), date_utc,
         from_addr, to_json, in_reply_to, references,
         str(body_path.relative_to(tmp))))
    return int(cur.lastrowid)


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
                   conn.execute("SELECT message_id, thread_id FROM items")}
        assert threads["<a@x>"] == threads["<b@x>"] == threads["<c@x>"]
        assert threads["<d@x>"] != threads["<a@x>"]
        rep = conn.execute(
            "SELECT representative_subject, item_count FROM threads"
            " WHERE id = ?", (threads["<a@x>"],)).fetchone()
        assert rep["item_count"] == 3 and \
            rep["representative_subject"] == "Settlement"

        # attachment text artifact + a skipped one (never chunked)
        att_txt = tmp / "att" / "1.txt"
        att_txt.parent.mkdir(parents=True)
        att_txt.write_text("Statement line items text.", encoding="utf-8")
        conn.execute(
            "INSERT INTO attachments (item_id, filename, sha256,"
            " extraction_method, extracted_text_path) VALUES"
            " (?, 's.pdf', 'x', 'ocrmypdf_redo_clean_pdftotext_layout', ?)",
            (a, str(att_txt.relative_to(tmp))))
        conn.execute(
            "INSERT INTO attachments (item_id, filename, sha256,"
            " extraction_method, is_skipped, skip_reason) VALUES"
            " (?, 'logo.png', 'y', 'stored_only', 1, 'image')", (a,))

        # Native PDF text continues through body_text_path/source_type
        # 'email_body', but receives a Document: filename payload.
        doc_path = tmp / "docs" / "notice.txt"
        doc_path.parent.mkdir(parents=True)
        doc_path.write_text("Native filing content.", encoding="utf-8")
        doc = conn.execute(
            """INSERT INTO items (item_kind, message_id, subject,
                      body_text_path, ingested_at)
               VALUES ('file', '<file@x>', 'notice.pdf', ?, 't')""",
            (str(doc_path.relative_to(tmp)),)).lastrowid
        conn.execute(
            """INSERT INTO item_memberships
                      (item_id, workspace_id, collection_id, filename,
                       sha256, membership_kind, ingested_at)
               VALUES (?, 'matter-x', 'mail', 'notice.pdf', 'native-sha',
                       'file', 't')""", (doc,))
        conn.commit()

        # -- embedding -------------------------------------------------------
        backend = FakeBackend()
        with patch.object(embed_mod, "current_fingerprint",
                          return_value=dict(FINGERPRINT)), \
             patch.object(embed_mod, "get_backend",
                          return_value=backend):
            estats = EmbedStage(ctx).run()
        assert estats.get("new_chunks") == 6, estats
        assert estats.get("payloads_updated") == 6, estats
        assert estats.get("embedded") == 6, estats
        assert estats.get("failed") == 0, estats
        assert estats.get("index_size") == 6, estats

        payloads = conn.execute(
            """SELECT chunks.text, chunks.char_start, chunks.char_end,
                      chunks.payload_shadow, chunks.source_type,
                      items.message_id
                 FROM chunks JOIN items ON items.id = chunks.item_id
                ORDER BY chunks.id""").fetchall()
        reply_chunk = next(row for row in payloads
                           if row["message_id"] == "<b@x>")
        assert reply_chunk["text"] == \
            "Reply text про Ксению и договор."
        assert reply_chunk["payload_shadow"].startswith(
            "From: bob@y\nDate: 2024-01-02T10:00:00+00:00\n"
            "Subject: Re: Settlement\nTo: alice@x\n\n")
        message_path = tmp / conn.execute(
            "SELECT body_text_path FROM items WHERE id = ?", (b,)).fetchone()[0]
        authored = body_text(message_path.read_bytes(), source=message_path)
        assert authored[reply_chunk["char_start"]:
                        reply_chunk["char_end"]] == reply_chunk["text"]
        attachment_chunk = next(row for row in payloads
                                if row["source_type"] == "attachment")
        assert attachment_chunk["payload_shadow"].startswith(
            "Attachment: s.pdf\nFrom: alice@x\nDate: ")
        document_chunk = next(row for row in payloads
                              if row["message_id"] == "<file@x>")
        assert document_chunk["payload_shadow"] == \
            "Document: notice.pdf\n\nNative filing content."
        assert set(backend.embedded_texts) == {
            row["payload_shadow"] for row in payloads}
        fts_envelope = conn.execute(
            "SELECT COUNT(*) FROM chunks_fts WHERE chunks_fts"
            " MATCH 'Settlement'").fetchone()[0]
        assert fts_envelope >= 3

        shadow = conn.execute(
            "SELECT translit_shadow FROM chunks WHERE item_id = ? AND"
            " source_type = 'email_body'", (b,)).fetchone()[0]
        assert "Kseni" in shadow, shadow   # proper-noun shadow present

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

        # Failure is retried on the next run (per-chunk cache semantics).
        e = insert_item(conn, tmp, "<e@x>", "Late arrival", "New text.",
                        "2024-04-01T10:00:00+00:00", "alice@x", "bob@y")
        conn.commit()
        with patch.object(embed_mod, "current_fingerprint",
                          return_value=dict(FINGERPRINT)), \
             patch.object(embed_mod, "get_backend",
                          return_value=FakeBackend(
                              fail_texts={"New text."})):
            estats3 = EmbedStage(ctx).run()
        assert estats3.get("failed") == 1 and \
            estats3.get("index_size") == 6, estats3
        with patch.object(embed_mod, "current_fingerprint",
                          return_value=dict(FINGERPRINT)), \
             patch.object(embed_mod, "get_backend",
                          return_value=FakeBackend()):
            estats4 = EmbedStage(ctx).run()
        assert estats4.get("embedded") == 1 and \
            estats4.get("index_size") == 7, estats4

        conn.close()
    print("test_thread_embed: all ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
