"""Self-test: new CLI grammar, orchestration, gates, legacy adapter seam."""
import contextlib
import io
import subprocess
import sys
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

import modules.cli as cli  # noqa: E402


ROOT = Path(__file__).resolve().parents[2]


class FakeConnection:
    def __init__(self):
        self.closed = False

    def close(self) -> None:
        self.closed = True


class FakeRegistry:
    def __init__(self, has_bank: bool):
        self._collections = (
            SimpleNamespace(is_bank_transactions=has_bank),)

    def active_collections(self):
        return self._collections


def fake_context(*, embed: bool, bank: bool):
    return SimpleNamespace(
        config=SimpleNamespace(embed_text=embed),
        registry=FakeRegistry(bank),
        conn=FakeConnection(),
    )


def parse_must_fail(arguments: list[str]) -> None:
    parser = cli.build_parser(lambda _: 0)
    with contextlib.redirect_stderr(io.StringIO()):
        try:
            parser.parse_args(arguments)
            raise AssertionError(f"arguments unexpectedly accepted: {arguments}")
        except SystemExit as exc:
            assert exc.code != 0


def test_grammar() -> None:
    parser = cli.build_parser(lambda _: 0)
    assert parser.parse_args(["ingest"]).stage == "all"
    assert parser.parse_args(["ingest", "discover"]).stage == "discover"
    assert parser.parse_args(["ingest", "transactions"]).stage == \
        "transactions"
    assert parser.parse_args(["transactions", "report"]).action == "report"
    assert parser.parse_args(["blob-index", "list-sources"]).action == \
        "list-sources"

    # Clean break: removed spellings and flags are errors, not aliases.
    parse_must_fail(["ingest", "parse"])
    parse_must_fail(["ingest", "documents"])
    parse_must_fail(["ingest", "attachments"])
    parse_must_fail(["ingest", "--embed", "text"])
    parse_must_fail(["transactions", "parse"])
    parse_must_fail(["transactions", "link"])
    parse_must_fail(["blob-index", "rebuild"])


def test_orchestration() -> None:
    executed: list[str] = []
    context = fake_context(embed=True, bank=True)
    with patch.object(cli, "_open_context", return_value=context), \
            patch.object(cli, "_execute_stage",
                         side_effect=lambda _, name: executed.append(name)):
        assert cli.run_ingest("all") == 0
    assert executed == ["discover", "emails", "pdfs", "thread", "embed",
                        "transactions"]
    assert context.conn.closed

    # Config gates apply only to `all`.
    executed.clear()
    context = fake_context(embed=False, bank=False)
    with patch.object(cli, "_open_context", return_value=context), \
            patch.object(cli, "_execute_stage",
                         side_effect=lambda _, name: executed.append(name)), \
            contextlib.redirect_stdout(io.StringIO()):
        cli.run_ingest("all")
    assert executed == ["discover", "emails", "pdfs", "thread"]
    assert context.conn.closed

    executed.clear()
    context = fake_context(embed=False, bank=False)
    with patch.object(cli, "_open_context", return_value=context), \
            patch.object(cli, "_execute_stage",
                         side_effect=lambda _, name: executed.append(name)):
        cli.run_ingest("embed")
    assert executed == ["embed"]

    executed.clear()
    context = fake_context(embed=False, bank=False)
    with patch.object(cli, "_open_context", return_value=context), \
            patch.object(cli, "_execute_stage",
                         side_effect=lambda _, name: executed.append(name)):
        cli.run_ingest("transactions")
    assert executed == ["transactions"]


def test_legacy_dispatch_seam() -> None:
    received = []

    def dispatch(args) -> int:
        received.append(args)
        return 7

    result = cli.main(
        ["query", "synthetic question", "--top-k", "3"],
        legacy_dispatch=dispatch,
    )
    assert result == 7
    assert received[0].command == "query"
    assert received[0].question == "synthetic question"
    assert received[0].top_k == 3


def test_entrypoint_bootstrap() -> None:
    result = subprocess.run(
        [str(ROOT / "pocket-advisor.py"), "--help"],
        cwd=ROOT,
        capture_output=True,
        text=True,
        check=False,
    )
    assert result.returncode == 0, result.stderr
    assert "ingest" in result.stdout
    assert "all | discover | emails | pdfs" in result.stdout


def main() -> int:
    test_grammar()
    test_orchestration()
    test_legacy_dispatch_seam()
    test_entrypoint_bootstrap()
    print("test_cli: all ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
