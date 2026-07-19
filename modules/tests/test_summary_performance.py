"""Self-test: one-shot/hierarchical summary execution and safe overflow."""
import re
import sys
import tempfile
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

import modules.pipeline.summaries as summaries_mod  # noqa: E402
import modules.pipeline.embed as embed_mod  # noqa: E402
import modules.retrieval as retrieval_mod  # noqa: E402
from modules.accuracy import run_expectations  # noqa: E402
from modules.config import Config  # noqa: E402
from modules.database import Database  # noqa: E402
from modules.pipeline.base import PipelineContext  # noqa: E402
from modules.pipeline.embed import EmbedStage  # noqa: E402
from modules.pipeline.summaries import (ThreadSummaryStage,  # noqa: E402
                                        _split_text_token_aware)
from modules.pipeline.thread import ThreadStage  # noqa: E402
from modules.review import ReviewLog  # noqa: E402
from modules.workspace import Registry  # noqa: E402

from test_embedding_design import (FINGERPRINT, REGISTRY_YAML,  # noqa: E402
                                   FakeEmbedder, insert_email)


class AnchorSummarizer:
    """Character-count tokenizer makes strategy boundaries exact in tests."""

    def __init__(self):
        self.modes: list[str] = []

    @staticmethod
    def count_tokens(text: str) -> int:
        return len(text)

    def generate(self, evidence: str, mode: str) -> str:
        self.modes.append(mode)
        positions = list(dict.fromkeys(re.findall(
            r"(?:ANCHOR_|nav)(EARLY|MIDDLE|LATE)", evidence,
            flags=re.IGNORECASE)))
        return " ".join(f"nav{position.lower()}fact"
                        for position in positions) \
            or f"summary-{len(self.modes)}"


def main() -> int:
    # The last-resort oversized-message split is deterministic, token-bounded,
    # Unicode-safe, and exactly lossless at the Python-text boundary.
    source = ("абвгд long evidence line\n" * 23) + "TAIL"
    pieces = _split_text_token_aware(source, 97, len)
    assert len(pieces) > 1
    assert "".join(pieces) == source
    assert all(len(piece) <= 97 for piece in pieces)
    assert pieces == _split_text_token_aware(source, 97, len)

    with tempfile.TemporaryDirectory(prefix="pa_summary_perf_") as td:
        root = Path(td)
        workspaces = root / "workspaces"
        workspaces.mkdir()
        (workspaces / "workspace-config.yaml").write_text(REGISTRY_YAML)
        base = Config(project_root=root, workspaces_dir=workspaces,
                      rerank_enabled=False)
        registry = Registry.load(base)
        workspace = registry.require_workspace("matter-x")
        cfg = base.for_workspace(workspace.id)
        conn = Database(cfg.db_path, workspace.id).open()
        ctx = PipelineContext(
            config=cfg, registry=registry, workspace=workspace, conn=conn,
            review=ReviewLog(conn, cfg.review_queue_csv))

        filler = "repeatable synthetic evidence " * 690
        first = insert_email(
            conn, root, "<long-a@x>", "Long chronology",
            "ANCHOR_EARLY\n" + filler,
            "2025-01-01T10:00:00+00:00", "a@x", "b@y")
        second = insert_email(
            conn, root, "<long-b@x>", "Re: Long chronology",
            "ANCHOR_MIDDLE\n" + filler,
            "2025-02-01T10:00:00+00:00", "b@y", "a@x",
            "<long-a@x>", "<long-a@x>")
        insert_email(
            conn, root, "<long-c@x>", "Re: Long chronology",
            "ANCHOR_LATE\n" + filler,
            "2025-03-01T10:00:00+00:00", "a@x", "b@y",
            "<long-b@x>", "<long-a@x> <long-b@x>")
        conn.commit()
        ThreadStage(ctx).run()

        generator = AnchorSummarizer()
        ctx.telemetry.mark_entered("summaries")
        with patch.object(summaries_mod, "get_summary_generator",
                          return_value=generator):
            stats = ThreadSummaryStage(ctx).run()
        ctx.telemetry.mark_measured("summaries")

        assert stats.get("generated") == 1, stats
        assert stats.get("hierarchical") == 1, stats
        assert generator.modes == [
            "segment", "segment", "segment", "reduce"], generator.modes
        thread_id = conn.execute(
            "SELECT thread_id FROM emails WHERE id=?", (first,)).fetchone()[0]
        summary = conn.execute(
            "SELECT summary_text, prompt_version FROM thread_summaries"
            " WHERE thread_id=?", (thread_id,)).fetchone()
        assert summary["prompt_version"] == 2
        assert summary["summary_text"] == \
            "navearlyfact navmiddlefact navlatefact"

        perf = ctx.telemetry.summaries
        assert perf.one_shot_threads == 0
        assert perf.hierarchical_threads == 1
        assert perf.input_segments == 3
        assert perf.generation_calls == 4
        assert perf.overflow_reductions == 1
        assert [tier.upper_bound_tokens for tier in perf.length_tiers] == \
            [8192, 48000, None]
        assert [tier.threads for tier in perf.length_tiers] == [0, 0, 1]
        ctx.telemetry.validate()

        # The middle message is a real direct reply and the late message
        # remains in the same deterministic thread; hierarchy never rewrites
        # relational evidence to obtain its speedup.
        assert conn.execute(
            "SELECT reply_parent_email_id FROM emails WHERE id=?",
            (second,)).fetchone()[0] == first
        assert conn.execute(
            "SELECT COUNT(DISTINCT thread_id) FROM emails").fetchone()[0] == 1

        # Beginning/middle/end expectations are answerable through the
        # generated navigation namespace rather than by direct leaf echo.
        with patch.object(embed_mod, "current_fingerprint",
                          return_value=dict(FINGERPRINT)), \
             patch.object(embed_mod, "get_backend",
                          return_value=FakeEmbedder()):
            EmbedStage(ctx).run()
        expectations = root / "positional-expectations.yaml"
        expectations.write_text("synthetic positional expectations\n")
        entries = [
            {"id": f"position-{position}", "question": marker,
             "expect_thread_key": "<long-a@x>", "flags": ["thread-level"]}
            for position, marker in (
                ("early", "navearlyfact"),
                ("middle", "navmiddlefact"),
                ("late", "navlatefact"),
            )
        ]
        with patch.object(retrieval_mod, "current_fingerprint",
                          return_value=dict(FINGERPRINT)), \
             patch.object(retrieval_mod, "get_backend",
                          return_value=FakeEmbedder()):
            result = run_expectations(
                ctx, entries, [expectations], top_k=3,
                label="synthetic positional navigation")
        assert [row["verdict"] for row in result["questions"]] == \
            ["THREAD(sum)", "THREAD(sum)", "THREAD(sum)"], result
        conn.close()

    print("test_summary_performance: all ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
