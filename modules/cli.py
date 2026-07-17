"""Pocket Advisor's sole argparse surface and pipeline orchestrator.

New ingestion commands run typed stages directly. Retrieval and maintenance
commands remain temporarily delegated by ``pocket-advisor.py`` to the frozen
``scripts/`` implementation; this module receives that adapter as a callback
and never imports from the frozen tree.
"""
import argparse
import subprocess
import sys
from collections.abc import Callable
from pathlib import Path
from typing import Any

from modules.config import Config
from modules.database import Database
from modules.pipeline.base import PipelineContext, Stage
from modules.review import ReviewLog
from modules.workspace import Registry


ROOT = Path(__file__).resolve().parent.parent
INGEST_STAGES = (
    "all",
    "discover",
    "emails",
    "pdfs",
    "thread",
    "embed",
    "transactions",
)
type LegacyDispatch = Callable[[argparse.Namespace], int]


_HELP = {
    "db": "init — create the fresh SQLite schema",
    "fetch-model": "download configured MLX model repos",
    "ingest": "all | discover | emails | pdfs | thread | embed | transactions",
    "transactions": "report — statement integrity and reconciliation report",
    "query": "one-off retrieval query (temporarily frozen implementation)",
    "daemon": "serve | status | stop — session-warm query daemon",
    "wipe": "list | index | state — explicit derived-state deletion",
    "blob-index": "list-sources | lookup — custody path tooling",
    "verify": "integrity check",
    "accuracy": "run | compare | list — golden-set retrieval accuracy",
    "test": "run every modules/tests/test_*.py self-test",
}
_GROUPS = (
    ("setup", ("db", "fetch-model")),
    ("pipeline", ("ingest", "transactions")),
    ("retrieval", ("query", "daemon")),
    ("maintenance", ("wipe", "blob-index", "verify")),
    ("quality", ("accuracy", "test")),
)


def _epilog() -> str:
    lines = ["commands:"]
    for group, names in _GROUPS:
        lines.append(f"  {group}:")
        for name in names:
            lines.append(f"    {name:14} {_HELP[name]}")
    lines.extend(("", "Per-command flags: pocket-advisor.py <command> --help"))
    return "\n".join(lines)


def _open_context() -> PipelineContext:
    config = Config.load()
    registry = Registry.load(config)
    conn = Database(config.db_path).open()
    return PipelineContext(
        config=config,
        registry=registry,
        conn=conn,
        review=ReviewLog(conn, config.review_queue_csv),
    )


def _stage_class(name: str) -> type[Stage]:
    """Lazy stage imports keep help and frozen retrieval startup light."""
    match name:
        case "discover":
            from modules.pipeline.discover import DiscoverStage
            return DiscoverStage
        case "emails":
            from modules.pipeline.emails import EmailStage
            return EmailStage
        case "pdfs":
            from modules.pipeline.pdfs import PdfTextStage
            return PdfTextStage
        case "thread":
            from modules.pipeline.thread import ThreadStage
            return ThreadStage
        case "embed":
            from modules.pipeline.embed import EmbedStage
            return EmbedStage
        case "transactions":
            from modules.pipeline.transactions import TransactionsStage
            return TransactionsStage
        case _:
            raise ValueError(f"unknown pipeline stage: {name}")


def _execute_stage(ctx: PipelineContext, name: str) -> None:
    _stage_class(name)(ctx).execute()


def run_ingest(stage: str) -> int:
    """Execute one named stage or the complete ordered pipeline."""
    ctx = _open_context()
    try:
        if stage != "all":
            _execute_stage(ctx, stage)
            return 0

        for name in ("discover", "emails", "pdfs", "thread"):
            _execute_stage(ctx, name)
        if ctx.config.embed_text:
            _execute_stage(ctx, "embed")
        else:
            print("embed: skipped (ingestion.embed_text=false)")
        has_bank_collections = any(
            collection.is_bank_transactions
            for collection in ctx.registry.active_collections())
        if has_bank_collections:
            _execute_stage(ctx, "transactions")
        else:
            print("transactions: skipped (no mounted bank-transactions collections)")
        return 0
    finally:
        ctx.conn.close()


def _handle_db(_: argparse.Namespace) -> int:
    config = Config.load()
    conn = Database(config.db_path).open()
    conn.close()
    print(f"database initialized: {config.db_path}")
    return 0


def _handle_ingest(args: argparse.Namespace) -> int:
    return run_ingest(args.stage)


def _handle_transactions(_: argparse.Namespace) -> int:
    from modules.pipeline.transactions import report_transactions

    ctx = _open_context()
    try:
        report_transactions(ctx)
        return 0
    finally:
        ctx.conn.close()


def _handle_test(_: argparse.Namespace) -> int:
    tests = sorted((ROOT / "modules" / "tests").glob("test_*.py"))
    if not tests:
        print("pocket-advisor test: no modules/tests/test_*.py found")
        return 1
    failures: list[str] = []
    for test in tests:
        result = subprocess.run(
            [sys.executable, str(test)],
            capture_output=True,
            text=True,
            check=False,
        )
        passed = result.returncode == 0
        print(f"  [{'ok' if passed else 'FAIL'}] {test.name}")
        if passed:
            continue
        failures.append(test.name)
        tail = (result.stdout + result.stderr).strip().splitlines()[-15:]
        for line in tail:
            print(f"      {line}")
    print(
        f"pocket-advisor test: {len(tests) - len(failures)}/{len(tests)} "
        "passed"
        + (f" — FAILED: {', '.join(failures)}" if failures else ""))
    return 1 if failures else 0


def _legacy_handler(
        dispatch: LegacyDispatch | None,
) -> Callable[[argparse.Namespace], int]:
    def handle(args: argparse.Namespace) -> int:
        if dispatch is None:
            raise RuntimeError(
                f"legacy command {args.command!r} needs the entrypoint adapter")
        return int(dispatch(args) or 0)

    return handle


def build_parser(
        legacy_dispatch: LegacyDispatch | None = None,
) -> argparse.ArgumentParser:
    legacy = _legacy_handler(legacy_dispatch)
    parser = argparse.ArgumentParser(
        prog="pocket-advisor.py",
        description="Pocket Advisor — local evidence ingestion and retrieval",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=_epilog(),
    )
    commands = parser.add_subparsers(
        dest="command", metavar="command", required=True)

    command = commands.add_parser("db", help=_HELP["db"])
    command.add_argument("action", choices=("init",))
    command.set_defaults(handler=_handle_db)

    command = commands.add_parser("fetch-model", help=_HELP["fetch-model"])
    command.set_defaults(handler=legacy)

    command = commands.add_parser(
        "ingest",
        help=_HELP["ingest"],
        description=(
            "Run all pipeline stages or exactly one named stage. A named "
            "stage assumes earlier artifacts already exist."),
    )
    command.add_argument(
        "stage",
        nargs="?",
        choices=INGEST_STAGES,
        default="all",
        help="pipeline stage (default: all)",
    )
    command.set_defaults(handler=_handle_ingest)

    command = commands.add_parser(
        "transactions", help=_HELP["transactions"])
    command.add_argument("action", choices=("report",))
    command.set_defaults(handler=_handle_transactions)

    command = commands.add_parser("query", help=_HELP["query"])
    command.add_argument("question")
    command.add_argument("--after", help="only items on/after YYYY-MM-DD")
    command.add_argument("--before", help="only items on/before YYYY-MM-DD")
    command.add_argument("--thread", type=int, help="restrict to one thread")
    privilege = command.add_mutually_exclusive_group()
    privilege.add_argument(
        "--include-privileged", action="store_true", default=None)
    privilege.add_argument("--exclude-privileged", action="store_true")
    command.add_argument("--purpose")
    command.add_argument("--top-k", type=int, default=None)
    command.add_argument("--no-thread-context", action="store_true")
    command.add_argument("--json", action="store_true")
    command.add_argument("--no-daemon", action="store_true")
    command.add_argument("--require-daemon", action="store_true")
    command.set_defaults(handler=legacy)

    command = commands.add_parser("daemon", help=_HELP["daemon"])
    actions = command.add_subparsers(
        dest="action", metavar="action", required=True)
    serve = actions.add_parser("serve", help="run in foreground")
    serve.add_argument("--idle-sec", type=int, default=None)
    actions.add_parser("status")
    actions.add_parser("stop")
    command.set_defaults(handler=legacy, idle_sec=None)

    command = commands.add_parser("wipe", help=_HELP["wipe"])
    actions = command.add_subparsers(
        dest="action", metavar="action", required=True)
    actions.add_parser("list")
    wipe_index = actions.add_parser("index")
    wipe_index.add_argument("--text", metavar="SLUG")
    wipe_index.add_argument("--all-inactive", action="store_true")
    wipe_index.add_argument("--yes", action="store_true")
    wipe_index.add_argument("--force", action="store_true")
    wipe_state = actions.add_parser("state")
    wipe_state.add_argument("--yes", action="store_true")
    command.set_defaults(handler=legacy)

    command = commands.add_parser("blob-index", help=_HELP["blob-index"])
    actions = command.add_subparsers(
        dest="action", metavar="action", required=True)
    actions.add_parser("list-sources")
    lookup = actions.add_parser("lookup")
    lookup.add_argument("--workspace", "-w", required=True)
    lookup.add_argument("--source", "-s", required=True)
    lookup.add_argument("--sha256", required=True)
    lookup.add_argument("--no-verify", action="store_true")
    command.set_defaults(handler=legacy)

    command = commands.add_parser("verify", help=_HELP["verify"])
    command.set_defaults(handler=legacy)

    command = commands.add_parser("accuracy", help=_HELP["accuracy"])
    actions = command.add_subparsers(
        dest="action", metavar="action", required=True)
    run = actions.add_parser("run")
    run.add_argument("--golden", required=True)
    run.add_argument("--label", default="run")
    run.add_argument("--top-k", type=int, default=None)
    run.add_argument("--mode", choices=("warm", "cold"), default="warm")
    compare = actions.add_parser("compare")
    compare.add_argument("result_a")
    compare.add_argument("result_b")
    listing = actions.add_parser("list")
    listing.add_argument("--golden")
    command.set_defaults(handler=legacy)

    command = commands.add_parser("test", help=_HELP["test"])
    command.set_defaults(handler=_handle_test)
    return parser


def main(
        argv: list[str] | None = None,
        *,
        legacy_dispatch: LegacyDispatch | None = None,
) -> int:
    args = build_parser(legacy_dispatch).parse_args(argv)
    handler: Callable[[argparse.Namespace], Any] = args.handler
    return int(handler(args) or 0)
