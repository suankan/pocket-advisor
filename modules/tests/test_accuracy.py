"""Self-test: native retrieval-expectation accuracy suite."""
import json
import sys
import tempfile
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

import modules.accuracy as accuracy_mod  # noqa: E402
import modules.pipeline.embed as embed_mod  # noqa: E402
import modules.pipeline.summaries as summaries_mod  # noqa: E402
import modules.retrieval as retrieval_mod  # noqa: E402
from modules.accuracy import (ExpectationError, expectation_files,  # noqa: E402
                              format_compare, format_list, format_run,
                              generate_expectations, load_expectations,
                              load_result, persist_result, result_files,
                              run_expectations, suite_paths)
from modules.config import Config  # noqa: E402
from modules.database import Database  # noqa: E402
from modules.embedding import PAYLOAD_RECIPE  # noqa: E402
from modules.pipeline.base import PipelineContext  # noqa: E402
from modules.pipeline.embed import EmbedStage  # noqa: E402
from modules.pipeline.summaries import ThreadSummaryStage  # noqa: E402
from modules.pipeline.thread import ThreadStage  # noqa: E402
from modules.question_generation import accept_question  # noqa: E402
from modules.review import ReviewLog  # noqa: E402
from modules.workspace import Registry  # noqa: E402

from test_embedding_design import (FakeEmbedder, FakeSummarizer,  # noqa: E402
                                   REGISTRY_YAML, insert_email)

DIM = 4
FINGERPRINT = {"backend": "mlx", "model": "fake/model", "dim": DIM,
               "chunk_chars": 1500, "chunk_overlap": 200,
               "payload_recipe": PAYLOAD_RECIPE}

EXPECTATIONS_YAML = """\
- id: q-strong
  question: "What did the direct reply say?"
  expect_any:
    - "<b@x>"
- id: q-thread
  question: "Which conversation is the project discussion?"
  expect_thread_key: "<a@x>"
  flags: [thread-level]
- id: q-todo
  question: "TODO — not written yet"
  expect_any:
    - "<a@x>"
- id: q-invalid
  question: "Anchored on a message this corpus does not contain?"
  expect_any:
    - "<missing@nowhere>"
"""


class FakeQuestionGenerator:
    model_id = "fake/question-model"

    def count_tokens(self, text: str) -> int:
        return max(1, len(text.split()))

    def truncate(self, text: str, max_tokens: int) -> str:
        words = text.split()
        if len(words) <= max_tokens:
            return text
        return " ".join(words[:max_tokens])

    def generate(self, evidence: str) -> str:
        snippet = " ".join(evidence.split()[:6]) or "evidence"
        return f"What does the evidence say about {snippet}?"


def main() -> int:
    assert accept_question("TODO x") is None
    assert accept_question("") is None
    assert accept_question("  Why was X unpaid?  ") == "Why was X unpaid?"

    with tempfile.TemporaryDirectory(prefix="pa_accuracy_") as td:
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

        insert_email(conn, root, "<a@x>", "Project", "Opening note.",
                     "2024-01-01T10:00:00+00:00", "a@x", "b@y")
        insert_email(conn, root, "<b@x>", "Re: Project", "Direct reply.",
                     "2024-01-02T10:00:00+00:00", "b@y", "a@x",
                     "<a@x>", "<a@x>")
        conn.commit()
        ThreadStage(ctx).run()
        with patch.object(summaries_mod, "get_summary_generator",
                          return_value=FakeSummarizer()):
            ThreadSummaryStage(ctx).run()
        with patch.object(embed_mod, "current_fingerprint",
                          return_value=dict(FINGERPRINT)), \
             patch.object(embed_mod, "get_backend",
                          return_value=FakeEmbedder()):
            EmbedStage(ctx).run()

        paths = suite_paths(ctx.config)
        assert paths.expectations_dir == \
            cfg.state_dir / "search-accuracy-tests" / "expectations"
        assert paths.results_dir == \
            cfg.state_dir / "search-accuracy-tests" / "results"
        assert not (ctx.workspace.root / "search-accuracy-test").exists()

        # generate: body-based questions, durable anchors, --force gate.
        target = paths.expectations_dir / accuracy_mod.GENERATED_EXPECTATIONS_NAME
        fake = FakeQuestionGenerator()
        written, stats = generate_expectations(
            ctx, target, force=False, generator=fake)
        assert written == target and stats.generated == 1, stats
        generated = load_expectations([target])
        assert generated[0]["expect_thread_key"] == "<a@x>"
        assert generated[0]["origin"] == "generated"
        assert "generated" in generated[0]["flags"]
        assert not generated[0]["question"].upper().startswith("TODO")
        assert "Opening" in generated[0]["question"] \
            or "Direct" in generated[0]["question"] \
            or "evidence" in generated[0]["question"]
        assert "subject" not in generated[0].get("hint", "").lower()
        try:
            generate_expectations(ctx, target, force=False, generator=fake)
            raise AssertionError("generated overwrite must require --force")
        except ExpectationError as exc:
            assert "--force" in str(exc)
        generate_expectations(ctx, target, force=True, limit=1, generator=fake)

        # authored set: strong / thread-level / TODO-skip / invalid anchor.
        authored = paths.expectations_dir / "authored.yaml"
        authored.write_text(EXPECTATIONS_YAML)
        files = expectation_files(paths, authored)
        entries = load_expectations(files)
        assert len(entries) == 4

        with patch.object(retrieval_mod, "current_fingerprint",
                          return_value=dict(FINGERPRINT)), \
             patch.object(retrieval_mod, "get_backend",
                          return_value=FakeEmbedder()):
            result = run_expectations(
                ctx, entries, files, top_k=5, label="fixture run")

        verdicts = {q["id"]: q for q in result["questions"]}
        assert verdicts["q-strong"]["verdict"] == "STRONG" \
            and verdicts["q-strong"]["rank"] == 1, verdicts["q-strong"]
        assert verdicts["q-strong"]["matched"] == "<b@x>"
        assert verdicts["q-thread"]["verdict"].startswith("THREAD"), \
            verdicts["q-thread"]
        assert verdicts["q-todo"]["verdict"] == "SKIPPED"
        assert verdicts["q-invalid"]["verdict"] == "INVALID"
        aggregates = result["aggregates"]
        assert aggregates["scored"] == 2 and aggregates["miss"] == 0
        assert aggregates["skipped"] == 1 and aggregates["invalid"] == 1
        assert aggregates["thread_or_better_rate"] == 1.0
        assert result["environment"]["embed"]["model"] == "fake/model"
        assert result["environment"]["corpus"]["emails"] == 2
        assert result["expectations"]["count"] == 4
        assert "question_generator" not in result["environment"]

        # Generated suite records question_generator identity on run.
        gen_files = expectation_files(paths, target)
        gen_entries = load_expectations(gen_files)
        with patch.object(retrieval_mod, "current_fingerprint",
                          return_value=dict(FINGERPRINT)), \
             patch.object(retrieval_mod, "get_backend",
                          return_value=FakeEmbedder()):
            gen_result = run_expectations(
                ctx, gen_entries, gen_files, top_k=5, label="generated")
        assert gen_result["environment"]["question_generator"][
            "prompt_version"] == 1
        assert gen_result["aggregates"]["skipped"] == 0

        # persistence: JSON record round-trips; ordering by filename.
        first = persist_result(result, paths)
        assert first.is_file() and load_result(first)["label"] == \
            "fixture run"
        rendered = format_run(result, first)
        assert "STRONG" in rendered and "1 TODO skipped" in rendered \
            and "1 INVALID" in rendered

        with patch.object(retrieval_mod, "current_fingerprint",
                          return_value=dict(FINGERPRINT)), \
             patch.object(retrieval_mod, "get_backend",
                          return_value=FakeEmbedder()):
            second_result = run_expectations(
                ctx, entries, files, top_k=5, label="second")
        second = persist_result(second_result, paths)
        files_on_disk = result_files(paths)
        assert files_on_disk == sorted([first, second])

        comparison = format_compare(
            [load_result(first), load_result(second)],
            [first.name, second.name])
        assert "No per-question changes" in comparison

        # a synthetic regression is surfaced per-question.
        regressed = json.loads(second.read_text())
        for question in regressed["questions"]:
            if question["id"] == "q-strong":
                question["verdict"], question["rank"] = "MISS", None
        comparison = format_compare(
            [load_result(first), regressed], [first.name, "regressed"])
        assert "q-strong" in comparison and "MISS" in comparison

        listing = format_list(paths)
        assert first.name in listing and second.name in listing

        conn.close()

        # A suite path cannot redirect outside the selected flat state root.
        unsafe = base.for_workspace("unsafe")
        unsafe.state_dir.mkdir(parents=True)
        outside = root / "outside-accuracy"
        outside.mkdir()
        unsafe.accuracy_tests_dir.symlink_to(
            outside, target_is_directory=True)
        try:
            suite_paths(unsafe)
            raise AssertionError("symlinked accuracy suite must be refused")
        except ExpectationError as exc:
            assert "refusing suite path" in str(exc) or \
                "symlinked suite path" in str(exc)
    print("test_accuracy: all ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
