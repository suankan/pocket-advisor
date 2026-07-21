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
from modules.pipeline.summaries import ThreadSummaryStage  # noqa: E402
from modules.pipeline.summaries_core import (  # noqa: E402
    _ThreadWork, _split_text_token_aware)
from modules.pipeline.summary_dispatch import (  # noqa: E402
    EmailThreadsSummaryDispatcher, SummaryOutcome)
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

    def generate(self, body: str, mode: str) -> str:
        self.modes.append(mode)
        positions = list(dict.fromkeys(re.findall(
            r"(?:ANCHOR_|nav)(EARLY|MIDDLE|LATE)", body,
            flags=re.IGNORECASE)))
        return " ".join(f"nav{position.lower()}fact"
                        for position in positions) \
            or f"summary-{len(self.modes)}"


def main() -> int:
    # The last-resort oversized-message split is deterministic, token-bounded,
    # Unicode-safe, and exactly lossless at the Python-text boundary.
    source = ("абвгд long body line\n" * 23) + "TAIL"
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
                      rerank_enabled=False, embed_text=False)
        registry = Registry.load(base)
        workspace = registry.require_workspace("matter-x")
        cfg = base.for_workspace(workspace.id)
        conn = Database(cfg.db_path, workspace.id).open()
        ctx = PipelineContext(
            config=cfg, registry=registry, workspace=workspace, conn=conn,
            review=ReviewLog(conn, cfg.review_queue_csv))

        filler = "repeatable synthetic body " * 690
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
        # relational body to obtain its speedup.
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


def _make_job(root: Path, thread_id: int, text: str) -> "_ThreadWork":
    from modules.pipeline.summaries_core import _MessageSource
    path = root / f"msg-{thread_id}.txt"
    path.write_text(text, encoding="utf-8")
    return _ThreadWork(
        thread_id, f"key-{thread_id}", f"digest-{thread_id}",
        (_MessageSource(f"<m-{thread_id}@x>", "2025-01-01T00:00:00+00:00",
                        path),))


def _fake_ctx() -> PipelineContext:
    from modules.config import Config
    from modules.telemetry import PerformanceTelemetry
    from modules.workspace import Registry
    root = Path(tempfile.mkdtemp(prefix="pa_summary_ctx_"))
    (root / "workspaces").mkdir()
    (root / "workspaces" / "workspace-config.yaml").write_text(REGISTRY_YAML)
    base = Config(project_root=root, workspaces_dir=root / "workspaces",
                  rerank_enabled=False, embed_text=False)
    registry = Registry.load(base)
    workspace = registry.require_workspace("matter-x")
    cfg = base.for_workspace(workspace.id)
    return PipelineContext(config=cfg, registry=registry, workspace=workspace,
                           conn=None, review=None,
                           telemetry=PerformanceTelemetry())


def test_concurrent_generation() -> None:
    """Up to max_in_flight threads generate at once; DB settlement stays
    serialized on the main thread; telemetry merges per task."""
    import threading

    from modules.inference import InferenceUnavailable

    root = Path(tempfile.mkdtemp(prefix="pa_summary_conc_"))
    try:
        live: list[int] = []
        gate = threading.Event()
        release = threading.Event()
        lock = threading.Lock()

        class SlowGen:
            def count_tokens(self, text: str) -> int:
                return len(text)

            def generate(self, body: str, mode: str) -> str:
                with lock:
                    live.append(1)
                    concurrent = len(live)
                gate.set()
                release.wait(5.0)
                with lock:
                    live.pop()
                return f"summary-{body[:8]}"

        jobs = [_make_job(root, tid, f"body-body-{tid} " + "x" * 50)
                for tid in range(6)]
        ctx = _fake_ctx()
        dispatcher = EmailThreadsSummaryDispatcher(
            ctx, SlowGen(), max_in_flight=3)
        for job in jobs:
            assert dispatcher.submit(job)
        # At most max_in_flight generate calls run concurrently.
        gate.wait(5.0)
        with lock:
            assert len(live) <= 3, len(live)
        release.set()
        done, failed, skipped, outcomes = dispatcher.drain()
        dispatcher.close()
        assert (done, failed, skipped) == (6, 0, 0), (done, failed, skipped)
        assert len(outcomes) == 6
        assert all(o.summary_text for o in outcomes)
        assert all(o.metrics.strategy for o in outcomes)
    finally:
        import shutil
        shutil.rmtree(root, ignore_errors=True)
    print("test_concurrent_generation: all ok")


def test_unavailable_skips_all() -> None:
    """An unreachable endpoint marks every attempted thread skipped
    (pending gap), not failed; once down, submission stops."""
    import threading

    from modules.inference import InferenceUnavailable

    root = Path(tempfile.mkdtemp(prefix="pa_summary_down_"))
    try:
        begin = threading.Event()

        class DownGen:
            def count_tokens(self, text: str) -> int:
                return len(text)

            def generate(self, body: str, mode: str) -> str:
                begin.wait(5.0)
                raise InferenceUnavailable("oMLX down")

        jobs = [_make_job(root, tid, f"body-{tid}") for tid in range(4)]
        ctx = _fake_ctx()
        dispatcher = EmailThreadsSummaryDispatcher(ctx, DownGen())
        for job in jobs:
            dispatcher.submit(job)
        begin.set()
        done, failed, skipped, outcomes = dispatcher.drain()
        dispatcher.close()
        assert done == 0 and failed == 0, (done, failed, skipped)
        assert skipped == 4, (done, failed, skipped)
        assert dispatcher.unavailable is not None
    finally:
        import shutil
        shutil.rmtree(root, ignore_errors=True)
    print("test_unavailable_skips_all: all ok")


def test_per_thread_failure_isolated() -> None:
    """One thread raising a non-unavailable error is flagged; others
    still complete."""
    root = Path(tempfile.mkdtemp(prefix="pa_summary_fail_"))
    try:
        class MostlyGood:
            def __init__(self):
                self.calls = 0

            def count_tokens(self, text: str) -> int:
                return len(text)

            def generate(self, body: str, mode: str) -> str:
                self.calls += 1
                if "boom" in body:
                    raise RuntimeError("boom on this thread")
                return f"summary-{body[:8]}"

        jobs = [_make_job(root, tid,
                          f"boom-body" if tid == 2 else f"ok-body-{tid}")
                for tid in range(5)]
        ctx = _fake_ctx()
        dispatcher = EmailThreadsSummaryDispatcher(ctx, MostlyGood())
        for job in jobs:
            dispatcher.submit(job)
        done, failed, skipped, outcomes = dispatcher.drain()
        dispatcher.close()
        assert (done, failed, skipped) == (4, 1, 0), (done, failed, skipped)
        boom = [o for o in outcomes if o.error is not None]
        assert len(boom) == 1 and "boom" in boom[0].error
    finally:
        import shutil
        shutil.rmtree(root, ignore_errors=True)
    print("test_per_thread_failure_isolated: all ok")


if __name__ == "__main__":
    sys.exit(main() or test_concurrent_generation()
             or test_unavailable_skips_all()
             or test_per_thread_failure_isolated())
