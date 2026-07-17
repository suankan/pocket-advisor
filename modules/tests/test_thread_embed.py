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
    privileged: false
workspaces:
  - id: matter-x
    active: true
    collections:
      - id: mail
"""

DIM = 4
FINGERPRINT = {"backend": "mlx", "model": "fake/model", "dim": DIM,
               "chunk_chars": 1500, "chunk_overlap": 200}


class FakeBackend:
    def __init__(self, fail_texts: set[str] = frozenset()):
        self.fail_texts = fail_texts

    def embed_one(self, text: str, is_query: bool = False):
        if text in self.fail_texts:
            raise RuntimeError("simulated embed failure")
        seed = sum(text.encode()) % 97 + 1
        vec = np.arange(1, DIM + 1, dtype=np.float32) * seed
        return vec / np.linalg.norm(vec)


def insert_item(conn, tmp: Path, mid: str, subject: str, body: str,
                date_utc: str, from_addr: str, to_addr: str,
                in_reply_to: str | None = None,
                references: str | None = None) -> int:
    body_path = tmp / "bodies" / f"{mid.strip('<>').replace('@', '_')}.txt"
    body_path.parent.mkdir(parents=True, exist_ok=True)
    body_path.write_text(body, encoding="utf-8")
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
        cfg = Config(project_root=tmp, workspaces_dir=ws_dir)
        conn = Database(cfg.db_path).open()
        ctx = PipelineContext(
            config=cfg, registry=Registry.load(cfg), conn=conn,
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
            "SELECT representative_subject, email_count FROM threads"
            " WHERE id = ?", (threads["<a@x>"],)).fetchone()
        assert rep["email_count"] == 3 and \
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
        conn.commit()

        # -- embedding -------------------------------------------------------
        with patch.object(embed_mod, "current_fingerprint",
                          return_value=dict(FINGERPRINT)), \
             patch.object(embed_mod, "get_backend",
                          return_value=FakeBackend()):
            estats = EmbedStage(ctx).run()
        assert estats.get("new_chunks") == 5, estats   # 4 bodies + 1 att
        assert estats.get("embedded") == 5, estats
        assert estats.get("failed") == 0, estats
        assert estats.get("index_size") == 5, estats

        shadow = conn.execute(
            "SELECT translit_shadow FROM chunks WHERE item_id = ? AND"
            " source_type = 'email_body'", (b,)).fetchone()[0]
        assert "Kseni" in shadow, shadow   # proper-noun shadow present

        from modules.embedding import index_paths
        paths = index_paths(cfg, FINGERPRINT)
        matrix = np.load(paths.vectors_npy)
        ids = np.load(paths.vectors_ids_npy)
        assert matrix.shape == (5, DIM) and len(ids) == 5
        meta = json.loads(paths.meta_json.read_text())
        assert meta["count"] == 5 and meta["model"] == "fake/model"

        # Re-run: up to date, backend never loaded.
        def boom(*_a, **_k):
            raise AssertionError("backend must not load when up to date")
        with patch.object(embed_mod, "current_fingerprint",
                          return_value=dict(FINGERPRINT)), \
             patch.object(embed_mod, "get_backend", side_effect=boom):
            estats2 = EmbedStage(ctx).run()
        assert estats2.get("index_size") == 5, estats2
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
            estats3.get("index_size") == 5, estats3
        with patch.object(embed_mod, "current_fingerprint",
                          return_value=dict(FINGERPRINT)), \
             patch.object(embed_mod, "get_backend",
                          return_value=FakeBackend()):
            estats4 = EmbedStage(ctx).run()
        assert estats4.get("embedded") == 1 and \
            estats4.get("index_size") == 6, estats4

        conn.close()
    print("test_thread_embed: all ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
