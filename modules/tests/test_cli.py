"""Self-test: command-scoped workspace grammar and orchestration."""
import contextlib
import io
import subprocess
import sys
import tempfile
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

import modules.cli as cli  # noqa: E402
from modules.config import Config  # noqa: E402


ROOT = Path(__file__).resolve().parents[2]
WS = ("--workspace", "matter-x")


class FakeConnection:
    def __init__(self):
        self.closed = False

    def close(self) -> None:
        self.closed = True


def fake_selection(*, embed: bool = True, bank: bool = False):
    collections = (SimpleNamespace(is_bank_transactions=bank),)
    workspace = SimpleNamespace(id="matter-x", collections=collections)
    config = SimpleNamespace(
        embed_text=embed,
        summarize_threads=True,
        default_top_k=15,
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
        ("accuracy", "run", "--golden", "golden.yaml"),
        ("accuracy", "list"),
    )
    for action in bound_actions:
        assert parser.parse_args([*WS, *action]).workspace_required is True

    free_actions = (
        ("fetch-model",),
        ("test",),
        ("accuracy", "compare", "a.json", "b.json"),
    )
    for action in free_actions:
        assert parser.parse_args(action).workspace_required is False

    # Workspace-bound actions require the global selector before the command.
    main_must_fail(["ingest"], "--workspace ID is required")
    parse_must_fail(["ingest", "--workspace", "matter-x"])

    # Workspace-free actions reject a meaningless selector.
    main_must_fail([*WS, "fetch-model"], "not accepted")
    main_must_fail([*WS, "test"], "not accepted")
    main_must_fail(
        [*WS, "accuracy", "compare", "a.json", "b.json"],
        "not accepted",
    )

    # Clean break: removed spellings and flags are errors, not aliases.
    parse_must_fail([*WS, "ingest", "parse"])
    parse_must_fail([*WS, "ingest", "documents"])
    parse_must_fail([*WS, "ingest", "attachments"])
    parse_must_fail([*WS, "ingest", "--embed", "text"])
    parse_must_fail([*WS, "transactions", "parse"])
    parse_must_fail([*WS, "transactions", "link"])
    parse_must_fail([*WS, "blob-index", "rebuild"])


def test_orchestration() -> None:
    executed: list[str] = []
    selection = fake_selection(embed=True, bank=True)
    context = fake_context(embed=True, bank=True)
    with patch.object(cli, "_open_context", return_value=context), \
            patch.object(cli, "_execute_stage",
                         side_effect=lambda _, name: executed.append(name)):
        assert cli.run_ingest("all", selection) == 0
    assert executed == ["discover", "emails", "pdfs", "thread",
                        "summaries", "embed", "transactions"]
    assert context.conn.closed

    # Config gates apply only to `all`; summaries always maintains staleness.
    executed.clear()
    selection = fake_selection(embed=False, bank=False)
    context = fake_context(embed=False, bank=False)
    with patch.object(cli, "_open_context", return_value=context), \
            patch.object(cli, "_execute_stage",
                         side_effect=lambda _, name: executed.append(name)), \
            contextlib.redirect_stdout(io.StringIO()):
        cli.run_ingest("all", selection)
    assert executed == ["discover", "emails", "pdfs", "thread", "summaries"]
    assert context.conn.closed

    for stage in ("embed", "summaries", "transactions"):
        executed.clear()
        selection = fake_selection(embed=False, bank=False)
        context = fake_context(embed=False, bank=False)
        with patch.object(cli, "_open_context", return_value=context), \
                patch.object(cli, "_execute_stage",
                             side_effect=lambda _, name: executed.append(name)):
            cli.run_ingest(stage, selection)
        assert executed == [stage]


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


def test_frozen_commands_fail_closed() -> None:
    selection = fake_selection()
    with patch.object(cli, "_resolve_selection", return_value=selection):
        try:
            cli.main([*WS, "verify"])
            raise AssertionError("workspace-unsafe frozen command must abort")
        except SystemExit as exc:
            assert "refusing the frozen shared-state implementation" in str(exc)

    # File-addressed comparison is workspace-free even while its native port
    # remains unavailable; it must fail closed without resolving a registry.
    with patch.object(
            cli, "_resolve_selection",
            side_effect=AssertionError("workspace resolution must not run")):
        try:
            cli.main(["accuracy", "compare", "a.json", "b.json"])
            raise AssertionError("unported accuracy compare must abort")
        except SystemExit as exc:
            assert "refusing the frozen shared-state implementation" in str(exc)


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
    test_native_query_seam()
    test_frozen_commands_fail_closed()
    test_workspace_free_dispatch()
    with tempfile.TemporaryDirectory(prefix="pa_cli_") as td:
        test_unknown_workspace_has_no_side_effect(Path(td))
    test_entrypoint_bootstrap()
    print("test_cli: all ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
