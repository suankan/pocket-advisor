"""Self-test: stable thread anchors, summaries, and dual vector indexes."""
import importlib
import json
import sys
import tempfile
from dataclasses import replace
from pathlib import Path
from unittest.mock import patch

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

import modules.pipeline.embed as embed_mod  # noqa: E402
import modules.pipeline.summaries as summaries_mod  # noqa: E402
import modules.retrieval as retrieval_mod  # noqa: E402
from modules.config import Config  # noqa: E402
from modules.database import Database  # noqa: E402
from modules.embedding import (PAYLOAD_RECIPE, index_paths,  # noqa: E402
                               thread_index_paths)
from modules.pipeline.base import PipelineContext  # noqa: E402
from modules.pipeline.embed import EmbedStage  # noqa: E402
from modules.pipeline.summaries import ThreadSummaryStage  # noqa: E402
from modules.pipeline.thread import ThreadStage  # noqa: E402
from modules.review import ReviewLog  # noqa: E402
from modules.retrieval import SearchOptions, run_search  # noqa: E402
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


class FakeEmbedder:
    def embed_one(self, text: str, is_query: bool = False):
        seed = sum(text.encode()) % 97 + 1
        vec = np.arange(1, DIM + 1, dtype=np.float32) * seed
        return vec / np.linalg.norm(vec)


class FakeSummarizer:
    def __init__(self, fail_on: str | None = None):
        self.fail_on = fail_on
        self.calls = 0

    def update(self, current_summary: str, message_segment: str,
               segment_label: str) -> str:
        self.calls += 1
        if self.fail_on and self.fail_on in message_segment:
            raise RuntimeError("simulated summary failure")
        line = message_segment.split("\n\n", 1)[-1].strip()
        return " | ".join(part for part in (current_summary, line) if part)


def insert_email(conn, root: Path, mid: str, subject: str, body: str,
                 date: str, from_addr: str, to_addr: str,
                 in_reply_to: str | None = None,
                 references: str | None = None) -> int:
    folder = root / "cache" / mid.strip("<>").replace("@", "_")
    folder.mkdir(parents=True, exist_ok=True)
    message = folder / "email_message.txt"
    message.write_text(
        f"Date: {date}\nFrom: {from_addr}\nTo: {to_addr}\nCc: "
        f"\nSubject: {subject}\n\n{body}")
    to_json = json.dumps([{"name": None, "addr": to_addr}])
    cur = conn.execute(
        """INSERT INTO items
           (item_kind, message_id, subject, subject_normalized, date_utc,
            from_addr, to_addrs, cc_addrs, in_reply_to, references_raw,
            body_text_path, ingested_at)
           VALUES ('email', ?, ?, ?, ?, ?, ?, '[]', ?, ?, ?, 't')""",
        (mid, subject, subject.removeprefix("Re: ").lower(), date,
         from_addr, to_json, in_reply_to, references,
         str(message.relative_to(root))))
    item_id = int(cur.lastrowid)
    conn.execute(
        """INSERT INTO item_memberships
           (item_id, workspace_id, collection_id, filename, sha256,
            membership_kind, ingested_at)
           VALUES (?, 'matter-x', 'mail', ?, ?, 'email', 't')""",
        (item_id, f"{mid}.eml", mid))
    return item_id


def main() -> int:
    with tempfile.TemporaryDirectory(prefix="pa_embedding_design_") as td:
        root = Path(td)
        workspaces = root / "workspaces"
        workspaces.mkdir()
        (workspaces / "workspace-config.yaml").write_text(REGISTRY_YAML)
        base = Config(project_root=root, workspaces_dir=workspaces,
                      rerank_enabled=False)
        registry = Registry.load(base)
        workspace = registry.require_workspace("matter-x")
        cfg = base.for_workspace(workspace.id)
        assert cfg.mlx_model_thread_summary == \
            "mlx-community/Qwen3.5-4B-MLX-4bit"
        importlib.import_module("mlx_lm.models.qwen3_5")
        conn = Database(cfg.db_path, workspace.id).open()
        ctx = PipelineContext(
            config=cfg, registry=registry, workspace=workspace, conn=conn,
            review=ReviewLog(conn, cfg.review_queue_csv))

        a = insert_email(conn, root, "<a@x>", "Project", "Opening note.",
                         "2024-01-01T10:00:00+00:00", "a@x", "b@y")
        b = insert_email(conn, root, "<b@x>", "Re: Project", "Direct reply.",
                         "2024-01-02T10:00:00+00:00", "b@y", "a@x",
                         "<a@x>", "<a@x>")
        c = insert_email(conn, root, "<c@x>", "Project",
                         "Headerless follow-up.",
                         "2024-01-03T10:00:00+00:00", "a@x", "c@z")
        conn.commit()

        ThreadStage(ctx).run()
        row_a = conn.execute(
            "SELECT thread_id FROM items WHERE id=?", (a,)).fetchone()
        thread_id = row_a["thread_id"]
        thread = conn.execute(
            "SELECT * FROM threads WHERE id=?", (thread_id,)).fetchone()
        assert thread["stable_key"] == "<a@x>"
        assert conn.execute(
            "SELECT reply_parent_item_id FROM items WHERE id=?",
            (b,)).fetchone()[0] == a
        assert conn.execute(
            "SELECT reply_parent_item_id FROM items WHERE id=?",
            (c,)).fetchone()[0] is None

        summarizer = FakeSummarizer()
        with patch.object(summaries_mod, "get_summary_generator",
                          return_value=summarizer):
            stats = ThreadSummaryStage(ctx).run()
        assert stats.get("generated") == 1 and summarizer.calls == 3, stats
        summary = conn.execute(
            "SELECT * FROM thread_summaries WHERE thread_id=?",
            (thread_id,)).fetchone()
        assert not summary["is_stale"]
        assert "Opening note" in summary["summary_text"]
        assert "Headerless follow-up" in summary["summary_text"]

        # Stable rerun: same thread id and no summary model load.
        ThreadStage(ctx).run()
        assert conn.execute(
            "SELECT thread_id FROM items WHERE id=?", (a,)).fetchone()[0] \
            == thread_id
        with patch.object(summaries_mod, "get_summary_generator",
                          side_effect=AssertionError("must not load")):
            stats2 = ThreadSummaryStage(ctx).run()
        assert stats2.get("unchanged") == 1, stats2

        # Both leaf and thread-summary matrices are built.
        with patch.object(embed_mod, "current_fingerprint",
                          return_value=dict(FINGERPRINT)), \
             patch.object(embed_mod, "get_backend",
                          return_value=FakeEmbedder()):
            estats = EmbedStage(ctx).run()
        assert estats.get("embedded_chunks") == 3, estats
        assert estats.get("embedded_threads") == 1, estats
        assert np.load(index_paths(cfg, FINGERPRINT).vectors_npy).shape \
            == (3, DIM)
        tpaths = thread_index_paths(cfg, FINGERPRINT)
        assert np.load(tpaths.vectors_npy).shape == (1, DIM)
        old_vector = next(tpaths.vecs_dir.glob("*.npy")).name

        # Leaf and summary matches deduplicate to one relational packet. The
        # readable evidence includes the full chronology and real reply edge.
        with patch.object(retrieval_mod, "current_fingerprint",
                          return_value=dict(FINGERPRINT)), \
             patch.object(retrieval_mod, "get_backend",
                          return_value=FakeEmbedder()):
            found = run_search(ctx, "Direct reply", SearchOptions(top_k=3))
        assert found["retrieval"]["leaf_fts"] >= 1, found
        assert found["retrieval"]["thread_fts"] >= 1, found
        assert len(found["results"]) == 1, found
        packet = found["results"][0]
        assert packet["thread_id"] == thread_id
        assert packet["generated_summary"]
        assert any(match["snippet"] == "Direct reply."
                   for match in packet["matches"]), packet["matches"]
        assert all("Subject:" not in match["snippet"]
                   for match in packet["matches"])
        assert len(packet["messages"]) == 3
        reply = next(message for message in packet["messages"]
                     if message["item_id"] == b)
        assert reply["reply_parent_item_id"] == a
        assert "Subject: Re: Project" in reply["email_message"]

        # A changed thread marks the old summary stale on failure, then
        # regenerates it and replaces only that thread vector.
        insert_email(conn, root, "<d@x>", "Re: Project", "Fourth message.",
                     "2024-01-04T10:00:00+00:00", "b@y", "a@x",
                     "<b@x>", "<a@x> <b@x>")
        conn.commit()
        ThreadStage(ctx).run()
        failing = FakeSummarizer(fail_on="Fourth message")
        with patch.object(summaries_mod, "get_summary_generator",
                          return_value=failing):
            failed = ThreadSummaryStage(ctx).run()
        assert failed.get("failed") == 1, failed
        assert conn.execute(
            "SELECT is_stale FROM thread_summaries WHERE thread_id=?",
            (thread_id,)).fetchone()[0] == 1

        # Query filters an old dense summary vector immediately from the
        # authoritative stale bit, before the embed stage prunes the file.
        with patch.object(retrieval_mod, "current_fingerprint",
                          return_value=dict(FINGERPRINT)), \
             patch.object(retrieval_mod, "get_backend",
                          return_value=FakeEmbedder()):
            stale_query = run_search(
                ctx, "Opening note", SearchOptions(top_k=3))
        assert stale_query["retrieval"]["thread_fts"] == 0, stale_query
        assert stale_query["retrieval"]["thread_dense"] == 0, stale_query

        # A stale summary is removed from the searchable matrix even though
        # there is no replacement vector yet.
        with patch.object(embed_mod, "current_fingerprint",
                          return_value=dict(FINGERPRINT)), \
             patch.object(embed_mod, "get_backend",
                          return_value=FakeEmbedder()):
            stale_embed = EmbedStage(ctx).run()
        assert stale_embed.get("embedded_chunks") == 1, stale_embed
        assert stale_embed.get("embedded_threads") == 0, stale_embed
        assert np.load(tpaths.vectors_npy).shape == (0, DIM)
        assert not list(tpaths.vecs_dir.glob("*.npy"))

        with patch.object(summaries_mod, "get_summary_generator",
                          return_value=FakeSummarizer()):
            repaired = ThreadSummaryStage(ctx).run()
        assert repaired.get("generated") == 1, repaired
        with patch.object(embed_mod, "current_fingerprint",
                          return_value=dict(FINGERPRINT)), \
             patch.object(embed_mod, "get_backend",
                          return_value=FakeEmbedder()):
            estats2 = EmbedStage(ctx).run()
        assert estats2.get("embedded_chunks") == 0, estats2
        assert estats2.get("embedded_threads") == 1, estats2
        files = list(tpaths.vecs_dir.glob("*.npy"))
        assert len(files) == 1 and files[0].name != old_vector

        # F8.1 — ghost-root ordering: a reply referencing a root not yet
        # imported keys its thread on the missing root; importing the
        # root later joins the same thread, materializes the reply edge,
        # and the digest change triggers summary generation.
        r = insert_email(conn, root, "<r@x>", "Re: Ghost",
                         "Boundary fence quote.",
                         "2024-02-02T10:00:00+00:00", "a@x", "b@y",
                         "<g@x>", "<g@x>")
        conn.commit()
        ThreadStage(ctx).run()
        ghost_thread = conn.execute(
            "SELECT thread_id FROM items WHERE id=?", (r,)).fetchone()[0]
        assert conn.execute(
            "SELECT stable_key FROM threads WHERE id=?",
            (ghost_thread,)).fetchone()[0] == "<g@x>"
        assert conn.execute(
            "SELECT reply_parent_item_id FROM items WHERE id=?",
            (r,)).fetchone()[0] is None

        g = insert_email(conn, root, "<g@x>", "Ghost", "Fence agreed.",
                         "2024-02-01T10:00:00+00:00", "b@y", "a@x")
        conn.commit()
        ThreadStage(ctx).run()
        assert conn.execute(
            "SELECT thread_id FROM items WHERE id=?",
            (r,)).fetchone()[0] == ghost_thread
        assert conn.execute(
            "SELECT thread_id FROM items WHERE id=?",
            (g,)).fetchone()[0] == ghost_thread
        assert conn.execute(
            "SELECT reply_parent_item_id FROM items WHERE id=?",
            (r,)).fetchone()[0] == g
        with patch.object(summaries_mod, "get_summary_generator",
                          return_value=FakeSummarizer()):
            ghost_stats = ThreadSummaryStage(ctx).run()
        assert ghost_stats.get("generated") == 1 and \
            ghost_stats.get("unchanged") == 1, ghost_stats

        # F1 — with generation disabled, staleness maintenance still
        # runs: a changed thread is marked stale and leaves retrieval,
        # no model is ever loaded, and re-enabling regenerates it.
        insert_email(conn, root, "<e@x>", "Re: Project", "Fifth message.",
                     "2024-01-05T10:00:00+00:00", "a@x", "b@y",
                     "<d@x>", "<a@x> <b@x> <d@x>")
        conn.commit()
        ThreadStage(ctx).run()
        off_ctx = PipelineContext(
            config=replace(cfg, summarize_threads=False),
            registry=ctx.registry, workspace=ctx.workspace,
            conn=conn, review=ctx.review)
        with patch.object(summaries_mod, "get_summary_generator",
                          side_effect=AssertionError("must not load")):
            off_stats = ThreadSummaryStage(off_ctx).run()
        assert off_stats.get("generation_disabled") == 1, off_stats
        assert not off_stats.get("generated"), off_stats
        assert conn.execute(
            "SELECT is_stale FROM thread_summaries WHERE thread_id=?",
            (thread_id,)).fetchone()[0] == 1
        with patch.object(retrieval_mod, "current_fingerprint",
                          return_value=dict(FINGERPRINT)), \
             patch.object(retrieval_mod, "get_backend",
                          return_value=FakeEmbedder()):
            off_query = run_search(ctx, "Opening note",
                                   SearchOptions(top_k=3))
        assert off_query["retrieval"]["thread_fts"] == 0, off_query
        with patch.object(summaries_mod, "get_summary_generator",
                          return_value=FakeSummarizer()):
            back_on = ThreadSummaryStage(ctx).run()
        assert back_on.get("generated") == 1, back_on

        conn.close()
    print("test_embedding_design: all ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
