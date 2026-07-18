"""Self-test: command-scoped workspace grammar and orchestration."""
import contextlib
import io
import subprocess
import sys
import tempfile
from datetime import datetime, timezone
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

import modules.cli as cli  # noqa: E402
from modules.config import Config  # noqa: E402
from modules.domain import StageStats  # noqa: E402


ROOT = Path(__file__).resolve().parents[2]
WS = ("--workspace", "matter-x")


class FakeConnection:
    def __init__(self):
        self.closed = False

    def close(self) -> None:
        self.closed = True

    def execute(self, _sql: str):
        return SimpleNamespace(fetchone=lambda: (0,))

    def rollback(self) -> None:
        pass


def fake_selection(*, embed: bool = True, bank: bool = False):
    collections = (SimpleNamespace(is_bank_transactions=bank),)
    workspace = SimpleNamespace(id="matter-x", collections=collections)
    config = SimpleNamespace(
        embed_text=embed,
        summarize_threads=True,
        default_top_k=15,
        daemon_auto=False,
    )
    return SimpleNamespace(config=config, registry=SimpleNamespace(),
                           workspace=workspace)


def fake_context(*, embed: bool, bank: bool):
    selection = fake_selection(embed=embed, bank=bank)
    return SimpleNamespace(
        config=selection.config,
        registry=selection.registry,
        workspace=selection.workspace,
        conn=FakeConnection(),
    )


def parse_must_fail(arguments: list[str]) -> None:
    parser = cli.build_parser()
    with contextlib.redirect_stderr(io.StringIO()):
        try:
            parser.parse_args(arguments)
            raise AssertionError(f"arguments unexpectedly accepted: {arguments}")
        except SystemExit as exc:
            assert exc.code != 0


def main_must_fail(arguments: list[str], expected: str) -> None:
    stderr = io.StringIO()
    with contextlib.redirect_stderr(stderr):
        try:
            cli.main(arguments)
            raise AssertionError(f"arguments unexpectedly accepted: {arguments}")
        except SystemExit as exc:
            assert exc.code != 0
            assert expected in stderr.getvalue()


def test_grammar() -> None:
    parser = cli.build_parser()
    assert parser.parse_args([*WS, "ingest"]).stage == "all"
    assert parser.parse_args([*WS, "ingest", "discover"]).stage == \
        "discover"
    assert parser.parse_args([*WS, "ingest", "transactions"]).stage == \
        "transactions"
    assert parser.parse_args([*WS, "transactions", "report"]).action == \
        "report"
    assert parser.parse_args([*WS, "blob-index", "list-sources"]).action == \
        "list-sources"
    bound_actions = (
        ("db", "init"),
        ("ingest",),
        ("transactions", "report"),
        ("query", "question"),
        ("daemon", "status"),
        ("wipe", "list"),
        ("wipe", "index", "--text", "slug"),
        ("wipe", "state"),
        ("blob-index", "list-sources"),
        ("blob-index", "lookup", "--source", "mail", "--sha256", "00"),
        ("verify",),
        ("accuracy", "generate"),
        ("accuracy", "run"),
        ("accuracy", "run", "--expectations", "set.yaml"),
        ("accuracy", "compare"),
        ("accuracy", "compare", "--last", "3"),
        ("accuracy", "list"),
    )
    for action in bound_actions:
        assert parser.parse_args([*WS, *action]).workspace_required is True

    free_actions = (
        ("fetch-model",),
        ("test",),
    )
    for action in free_actions:
        assert parser.parse_args(action).workspace_required is False

    # Workspace-bound actions require the global selector before the command.
    main_must_fail(["ingest"], "--workspace ID is required")
    parse_must_fail(["ingest", "--workspace", "matter-x"])

    # Workspace-free actions reject a meaningless selector.
    main_must_fail([*WS, "fetch-model"], "not accepted")
    main_must_fail([*WS, "test"], "not accepted")

    # Native accuracy compare takes --last, never result-file positionals.
    parse_must_fail([*WS, "accuracy", "compare", "a.json", "b.json"])
    parse_must_fail([*WS, "accuracy", "run", "--golden", "set.yaml"])

    # Clean break: removed spellings and flags are errors, not aliases.
    parse_must_fail([*WS, "ingest", "parse"])
    parse_must_fail([*WS, "ingest", "documents"])
    parse_must_fail([*WS, "ingest", "attachments"])
    parse_must_fail([*WS, "ingest", "--embed", "text"])
    parse_must_fail([*WS, "transactions", "parse"])
    parse_must_fail([*WS, "transactions", "link"])
    parse_must_fail([*WS, "blob-index", "rebuild"])
    parse_must_fail([
        *WS, "query", "question", "--no-daemon", "--require-daemon"])


def test_orchestration() -> None:
    def record_stage(executed: list[str], name: str) -> StageStats:
        executed.append(name)
        stats = StageStats()
        stats.inc("ran")
        return stats

    executed: list[str] = []
    selection = fake_selection(embed=True, bank=True)
    context = fake_context(embed=True, bank=True)
    with patch.object(cli, "_open_context", return_value=context), \
            patch.object(cli, "_execute_stage",
                         side_effect=lambda _, name:
                         record_stage(executed, name)), \
            patch.object(cli, "_finalize_ingest_report") as finalize:
        assert cli.run_ingest("all", selection) == 0
    assert executed == ["discover", "emails", "pdfs", "thread",
                        "summaries", "embed", "transactions"]
    stages = finalize.call_args.kwargs["stages"]
    assert [item.name for item in stages] == executed
    assert all(item.outcome == "completed" for item in stages)
    assert context.conn.closed

    # Config gates apply only to `all`; summaries always maintains staleness.
    executed.clear()
    selection = fake_selection(embed=False, bank=False)
    context = fake_context(embed=False, bank=False)
    with patch.object(cli, "_open_context", return_value=context), \
            patch.object(cli, "_execute_stage",
                         side_effect=lambda _, name:
                         record_stage(executed, name)), \
            patch.object(cli, "_finalize_ingest_report") as finalize, \
            contextlib.redirect_stdout(io.StringIO()):
        cli.run_ingest("all", selection)
    assert executed == ["discover", "emails", "pdfs", "thread", "summaries"]
    stages = finalize.call_args.kwargs["stages"]
    assert [(item.name, item.outcome) for item in stages[-2:]] == [
        ("embed", "skipped"), ("transactions", "skipped")]
    assert context.conn.closed

    for stage in ("embed", "summaries", "transactions"):
        executed.clear()
        selection = fake_selection(embed=False, bank=False)
        context = fake_context(embed=False, bank=False)
        with patch.object(cli, "_open_context", return_value=context), \
                patch.object(cli, "_execute_stage",
                             side_effect=lambda _, name:
                             record_stage(executed, name)):
            cli.run_ingest(stage, selection)
        assert executed == [stage]


def test_ingest_reporting_failures_and_timings() -> None:
    fixed_now = lambda: datetime(2026, 7, 18, tzinfo=timezone.utc)

    # A failed stage keeps its original exception, records completed timing,
    # and marks every downstream stage not_run.
    selection = fake_selection(embed=True, bank=True)
    context = fake_context(embed=True, bank=True)
    ticks = iter(float(value) for value in range(20))

    def fail_at_pdfs(_ctx, name: str) -> StageStats:
        if name == "pdfs":
            raise RuntimeError("synthetic pipeline failure")
        stats = StageStats()
        stats.inc("ok")
        return stats

    with patch.object(cli, "_open_context", return_value=context), \
            patch.object(cli, "_execute_stage", side_effect=fail_at_pdfs), \
            patch.object(cli, "_finalize_ingest_report") as finalize, \
            contextlib.redirect_stdout(io.StringIO()):
        try:
            cli.run_ingest(
                "all", selection, clock=lambda: next(ticks),
                utc_now=fixed_now)
            raise AssertionError("pipeline failure must propagate")
        except RuntimeError as exc:
            assert str(exc) == "synthetic pipeline failure"
    call = finalize.call_args.kwargs
    assert call["failed_stage"] == "pdfs"
    assert call["pipeline_seconds"] == 7.0
    assert [(item.name, item.outcome) for item in call["stages"]] == [
        ("discover", "completed"),
        ("emails", "completed"),
        ("pdfs", "failed"),
        ("thread", "not_run"),
        ("summaries", "not_run"),
        ("embed", "not_run"),
        ("transactions", "not_run"),
    ]
    assert [item.duration_seconds for item in call["stages"][:3]] == \
        [1.0, 1.0, 1.0]
    assert context.conn.closed

    # A required report failure does not roll back completed stages, but the
    # full command returns non-zero as locked by the reporting contract.
    selection = fake_selection(embed=False, bank=False)
    context = fake_context(embed=False, bank=False)

    def successful_stage(_ctx, _name: str) -> StageStats:
        return StageStats()

    with patch.object(cli, "_open_context", return_value=context), \
            patch.object(cli, "_execute_stage", side_effect=successful_stage), \
            patch.object(
                cli, "_finalize_ingest_report",
                side_effect=OSError("synthetic report failure")), \
            contextlib.redirect_stdout(io.StringIO()):
        assert cli.run_ingest("all", selection, utc_now=fixed_now) == 1
    assert context.conn.closed


def test_native_query_seam() -> None:
    selection = fake_selection(embed=False, bank=False)
    context = fake_context(embed=False, bank=False)
    received = []
    with patch.object(cli, "_resolve_selection", return_value=selection), \
            patch.object(cli, "_open_context", return_value=context), \
            patch("modules.retrieval.run_search",
                  side_effect=lambda _ctx, question, options:
                  received.append((question, options)) or {
                      "question": question, "results": [], "warnings": [],
                      "retrieval": {},
                  }), \
            patch("modules.retrieval.format_results"):
        result = cli.main(
            [*WS, "query", "synthetic question", "--top-k", "3"],
        )
    assert result == 0
    assert received[0][0] == "synthetic question"
    assert received[0][1].top_k == 3
    assert context.conn.closed

    # Auto mode uses the workspace-local daemon without opening a second DB;
    # --no-daemon preserves the exact cold path above.
    warm_selection = fake_selection(embed=False, bank=False)
    warm_selection.config.daemon_auto = True
    response = {
        "ok": True,
        "result": {"question": "warm", "results": [], "warnings": [],
                   "retrieval": {}},
    }
    with patch.object(cli, "_resolve_selection", return_value=warm_selection), \
            patch.object(
                cli, "_open_context",
                side_effect=AssertionError("warm query must not open DB")), \
            patch("modules.daemon.daemon_request", return_value=response), \
            patch("modules.retrieval.format_results") as formatter, \
            contextlib.redirect_stderr(io.StringIO()):
        assert cli.main([*WS, "query", "warm"]) == 0
    formatter.assert_called_once_with(response["result"], as_json=False)


def test_native_maintenance_handlers() -> None:
    parser = cli.build_parser()
    assert parser.parse_args([*WS, "daemon", "status"]).handler is \
        cli._handle_daemon
    assert parser.parse_args([*WS, "wipe", "list"]).handler is \
        cli._handle_wipe
    assert parser.parse_args([
        *WS, "blob-index", "list-sources"]).handler is cli._handle_blob_index
    assert parser.parse_args([*WS, "verify"]).handler is cli._handle_verify


def test_workspace_free_dispatch() -> None:
    with patch.object(
            cli, "_resolve_selection",
            side_effect=AssertionError("workspace resolution must not run")), \
            patch.object(cli, "_handle_test", return_value=0) as test_handler:
        assert cli.main(["test"]) == 0
        test_handler.assert_called_once()

    config = SimpleNamespace(
        models_dir=ROOT / "models",
        mlx_model_embed_text="fake/embed",
        mlx_model_rerank="fake/rerank",
        mlx_model_thread_summary="fake/summary",
        rerank_enabled=False,
        summarize_threads=False,
    )
    store = SimpleNamespace(
        snapshot_dir=lambda repo: ROOT / "models" / repo,
        embed_dim_for_repo=lambda _repo: 4,
    )
    with patch.object(cli.Config, "load", return_value=config), \
            patch.object(
                cli, "_resolve_selection",
                side_effect=AssertionError("workspace resolution must not run")), \
            patch("modules.embedding.loader.ModelStore", return_value=store), \
            contextlib.redirect_stdout(io.StringIO()):
        assert cli.main(["fetch-model"]) == 0


def test_ingest_report_display(tmp: Path) -> None:
    from modules.ingest_report import (Finding, IngestRunReport, StageRun,
                                       format_report, latest_report_path,
                                       load_report, persist_report)

    config = SimpleNamespace(project_root=tmp, logs_dir=tmp / "logs")
    report = IngestRunReport(
        schema_version=1,
        workspace_id="matter-x",
        started_at="2026-07-18T00:00:00+00:00",
        ended_at="2026-07-18T00:00:05+00:00",
        status="COMPLETE",
        pipeline_seconds=5.0,
        report_seconds=0.01,
        stages=[StageRun("discover", "completed", 1.0, {"new": 2})],
        snapshot=None,
        findings=[Finding("error", "pdf_failures", 2)],
    )
    path = persist_report(report, config)
    assert latest_report_path(config) == path
    loaded = load_report(path)
    assert loaded.workspace_id == "matter-x"
    assert loaded.stages[0].stats == {"new": 2}
    rendered = format_report(loaded, path)
    assert "INGEST COMPLETE" in rendered
    assert "pdf_failures=2" in rendered

    # CLI dispatch: `ingest report` (and --last) render the saved record;
    # an explicit path works too; stray report flags on real stages fail.
    selection = fake_selection()
    selection.config.project_root = tmp
    selection.config.logs_dir = tmp / "logs"
    with patch.object(cli, "_resolve_selection", return_value=selection):
        for arguments in ([*WS, "ingest", "report"],
                          [*WS, "ingest", "report", "--last"],
                          [*WS, "ingest", "report", str(path)]):
            out = io.StringIO()
            with contextlib.redirect_stdout(out):
                assert cli.main(arguments) == 0
            assert "pdf_failures=2" in out.getvalue()
        try:
            cli.main([*WS, "ingest", "report", "--last", str(path)])
            raise AssertionError("--last plus a path must conflict")
        except SystemExit as exc:
            assert "not both" in str(exc)
        try:
            cli.main([*WS, "ingest", "emails", "--last"])
            raise AssertionError("stray --last must be rejected")
        except SystemExit as exc:
            assert "ingest report" in str(exc)
        try:
            cli.main([*WS, "ingest", "report", "missing.json"])
            raise AssertionError("missing record must abort")
        except SystemExit as exc:
            assert "no such record" in str(exc)


def test_unknown_workspace_has_no_side_effect(tmp: Path) -> None:
    workspaces = tmp / "workspaces"
    workspaces.mkdir()
    (workspaces / "workspace-config.yaml").write_text("""\
schema_version: 2
collections:
  - id: mail
    path: corpora/mail
workspaces:
  - id: matter-x
    collections: [{id: mail}]
""")
    base = Config(project_root=tmp, workspaces_dir=workspaces)
    with patch.object(cli.Config, "load", return_value=base):
        try:
            cli.main(["--workspace", "missing", "verify"])
            raise AssertionError("unknown workspace must abort")
        except SystemExit as exc:
            assert "unknown workspace" in str(exc)
    assert not base.state_root.exists()


def test_entrypoint_bootstrap() -> None:
    result = subprocess.run(
        [str(ROOT / "pocket-advisor.py"), "--help"],
        cwd=ROOT,
        capture_output=True,
        text=True,
        check=False,
    )
    assert result.returncode == 0, result.stderr
    assert "--workspace" in result.stdout
    assert "all | discover | emails | pdfs" in result.stdout
    assert "Workspace-free:  pocket-advisor.py fetch-model | test" in result.stdout
    assert "accuracy compare A B" not in result.stdout

    for action in (("test",), ("ingest",), ("wipe", "state"),
                   ("accuracy", "compare")):
        state_free_help = subprocess.run(
            [str(ROOT / "pocket-advisor.py"), *action, "--help"],
            cwd=ROOT,
            capture_output=True,
            text=True,
            check=False,
        )
        assert state_free_help.returncode == 0, state_free_help.stderr

    missing = subprocess.run(
        [str(ROOT / "pocket-advisor.py"), "verify"],
        cwd=ROOT,
        capture_output=True,
        text=True,
        check=False,
    )
    assert missing.returncode != 0
    assert "--workspace ID is required" in missing.stderr


def main() -> int:
    test_grammar()
    test_orchestration()
    test_ingest_reporting_failures_and_timings()
    test_native_query_seam()
    test_native_maintenance_handlers()
    test_workspace_free_dispatch()
    with tempfile.TemporaryDirectory(prefix="pa_cli_report_") as td:
        test_ingest_report_display(Path(td))
    with tempfile.TemporaryDirectory(prefix="pa_cli_") as td:
        test_unknown_workspace_has_no_side_effect(Path(td))
    test_entrypoint_bootstrap()
    print("test_cli: all ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
