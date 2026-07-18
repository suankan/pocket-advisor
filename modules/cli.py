"""Pocket Advisor's sole argparse surface and pipeline orchestrator.

Ingestion, cold retrieval, and workspace-state wiping run through typed
``modules/`` code directly. Remaining frozen operational commands fail closed
until their native workspace-scoped ports land; this module never imports the
frozen tree.
"""
import argparse
import subprocess
import sys
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from modules.config import Config
from modules.database import Database
from modules.pipeline.base import PipelineContext, Stage
from modules.review import ReviewLog
from modules.workspace import Registry, Workspace


ROOT = Path(__file__).resolve().parent.parent
INGEST_STAGES = (
    "all",
    "discover",
    "emails",
    "pdfs",
    "thread",
    "summaries",
    "embed",
    "transactions",
)
@dataclass(frozen=True, slots=True)
class RuntimeSelection:
    """Validated workspace selection, resolved before any command effects."""

    config: Config
    registry: Registry
    workspace: Workspace


_HELP = {
    "db": "init — create the fresh SQLite schema",
    "fetch-model": "download configured MLX model repos (workspace-free)",
    "ingest": "all | discover | emails | pdfs | thread | summaries | embed | transactions",
    "transactions": "report — statement integrity and reconciliation report",
    "query": "one-off hybrid leaf/thread retrieval query",
    "daemon": "serve | status | stop — unavailable pending native port",
    "wipe": "state (native) | list/index (unavailable pending native port)",
    "blob-index": "list-sources | lookup — unavailable pending native port",
    "verify": "unavailable pending native port",
    "accuracy": (
        "run/list (workspace-bound) | compare (workspace-free) — unavailable"),
    "test": "run every modules/tests/test_*.py self-test (workspace-free)",
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
    lines.extend((
        "",
        "Workspace-bound: pocket-advisor.py --workspace <id> <command> ...",
        "Workspace-free:  pocket-advisor.py fetch-model | test | accuracy compare A B",
        "Help:            pocket-advisor.py <command> --help",
    ))
    return "\n".join(lines)


def _resolve_selection(workspace_id: str) -> RuntimeSelection:
    base_config = Config.load()
    registry = Registry.load(base_config)
    workspace = registry.require_workspace(workspace_id)
    return RuntimeSelection(
        config=base_config.for_workspace(workspace.id),
        registry=registry,
        workspace=workspace,
    )


def _open_context(selection: RuntimeSelection) -> PipelineContext:
    config = selection.config
    conn = Database(config.db_path, selection.workspace.id).open()
    return PipelineContext(
        config=config,
        registry=selection.registry,
        workspace=selection.workspace,
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
        case "summaries":
            from modules.pipeline.summaries import ThreadSummaryStage
            return ThreadSummaryStage
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


def run_ingest(stage: str, selection: RuntimeSelection) -> int:
    """Execute one named stage or the complete ordered pipeline."""
    ctx = _open_context(selection)
    try:
        if stage != "all":
            _execute_stage(ctx, stage)
            return 0

        # summaries always runs: its staleness maintenance must see every
        # ingest even when generation is disabled (the stage gates the
        # generative pass on ingestion.summarize_threads itself).
        for name in ("discover", "emails", "pdfs", "thread", "summaries"):
            _execute_stage(ctx, name)
        if ctx.config.embed_text:
            _execute_stage(ctx, "embed")
        else:
            print("embed: skipped (ingestion.embed_text=false)")
        has_bank_collections = any(
            collection.is_bank_transactions
            for collection in ctx.workspace.collections)
        if has_bank_collections:
            _execute_stage(ctx, "transactions")
        else:
            print("transactions: skipped (no mounted bank-transactions collections)")
        return 0
    finally:
        ctx.conn.close()


def _handle_db(args: argparse.Namespace) -> int:
    selection: RuntimeSelection = args.selection
    config = selection.config
    conn = Database(config.db_path, selection.workspace.id).open()
    conn.close()
    print(f"database initialized: {config.db_path}")
    return 0


def _handle_fetch_model(args: argparse.Namespace) -> int:
    from modules.embedding.loader import ModelStore

    _ = args
    config = Config.load()
    store = ModelStore(config.models_dir)
    embed = store.snapshot_dir(config.mlx_model_embed_text)
    print(f"Text embed model ready: {embed}")
    print(f"  embed dim: {store.embed_dim_for_repo(config.mlx_model_embed_text)}")
    if config.rerank_enabled:
        rerank = store.snapshot_dir(config.mlx_model_rerank)
        print(f"Rerank model ready: {rerank}")
    else:
        print("Rerank model: skipped (query.rerank_enabled=false)")
    if config.summarize_threads:
        summary = store.snapshot_dir(config.mlx_model_thread_summary)
        print(f"Thread summary model ready: {summary}")
    else:
        print("Thread summary model: skipped"
              " (ingestion.summarize_threads=false)")
    return 0


def _handle_ingest(args: argparse.Namespace) -> int:
    return run_ingest(args.stage, args.selection)


def _handle_transactions(args: argparse.Namespace) -> int:
    from modules.pipeline.transactions import report_transactions

    ctx = _open_context(args.selection)
    try:
        report_transactions(ctx)
        return 0
    finally:
        ctx.conn.close()


def _handle_query(args: argparse.Namespace) -> int:
    from modules.retrieval import SearchOptions, format_results, run_search

    if args.require_daemon:
        raise SystemExit(
            "query: the new relational retriever currently runs cold; "
            "--require-daemon is unavailable until the daemon port lands")
    ctx = _open_context(args.selection)
    try:
        top_k = args.top_k or ctx.config.default_top_k
        if top_k <= 0:
            raise SystemExit("query: --top-k must be positive")
        result = run_search(ctx, args.question, SearchOptions(
            top_k=top_k,
            after=args.after,
            before=args.before,
            thread_id=args.thread,
            purpose=args.purpose,
            expand_thread_context=not args.no_thread_context,
        ))
        format_results(result, as_json=args.json)
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


def _handle_wipe_state(args: argparse.Namespace) -> int:
    from modules.wipe import wipe_state

    selection: RuntimeSelection = args.selection
    return wipe_state(
        selection.config,
        selection.registry,
        selection.workspace,
        yes=args.yes,
    )


def _handle_workspace_unsafe(args: argparse.Namespace) -> int:
    action = f" {args.action}" if getattr(args, "action", None) else ""
    scope = "workspace-scoped " if args.workspace_required else ""
    raise SystemExit(
        f"{args.command}{action}: unavailable until its native {scope}"
        "port lands; refusing the frozen shared-state "
        "implementation")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="pocket-advisor.py",
        description="Pocket Advisor — local evidence ingestion and retrieval",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=_epilog(),
    )
    parser.add_argument(
        "--workspace",
        dest="workspace_id",
        metavar="ID",
        help="workspace id required only for workspace-bound actions",
    )
    commands = parser.add_subparsers(
        dest="command", metavar="command", required=True)

    command = commands.add_parser("db", help=_HELP["db"])
    command.add_argument("action", choices=("init",))
    command.set_defaults(handler=_handle_db, workspace_required=True)

    command = commands.add_parser("fetch-model", help=_HELP["fetch-model"])
    command.set_defaults(handler=_handle_fetch_model, workspace_required=False)

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
    command.set_defaults(handler=_handle_ingest, workspace_required=True)

    command = commands.add_parser(
        "transactions", help=_HELP["transactions"])
    command.add_argument("action", choices=("report",))
    command.set_defaults(handler=_handle_transactions, workspace_required=True)

    command = commands.add_parser("query", help=_HELP["query"])
    command.add_argument("question")
    command.add_argument("--after", help="only items on/after YYYY-MM-DD")
    command.add_argument("--before", help="only items on/before YYYY-MM-DD")
    command.add_argument("--thread", type=int, help="restrict to one thread")
    command.add_argument("--purpose")
    command.add_argument("--top-k", type=int, default=None)
    command.add_argument("--no-thread-context", action="store_true")
    command.add_argument("--json", action="store_true")
    command.add_argument("--no-daemon", action="store_true")
    command.add_argument("--require-daemon", action="store_true")
    command.set_defaults(handler=_handle_query, workspace_required=True)

    command = commands.add_parser("daemon", help=_HELP["daemon"])
    actions = command.add_subparsers(
        dest="action", metavar="action", required=True)
    serve = actions.add_parser("serve", help="run in foreground")
    serve.add_argument("--idle-sec", type=int, default=None)
    actions.add_parser("status")
    actions.add_parser("stop")
    command.set_defaults(
        handler=_handle_workspace_unsafe,
        idle_sec=None,
        workspace_required=True,
    )

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
    command.set_defaults(
        handler=_handle_workspace_unsafe, workspace_required=True)
    wipe_state.set_defaults(handler=_handle_wipe_state)

    command = commands.add_parser("blob-index", help=_HELP["blob-index"])
    actions = command.add_subparsers(
        dest="action", metavar="action", required=True)
    actions.add_parser("list-sources")
    lookup = actions.add_parser("lookup")
    lookup.add_argument("--source", "-s", required=True)
    lookup.add_argument("--sha256", required=True)
    lookup.add_argument("--no-verify", action="store_true")
    command.set_defaults(
        handler=_handle_workspace_unsafe, workspace_required=True)

    command = commands.add_parser("verify", help=_HELP["verify"])
    command.set_defaults(
        handler=_handle_workspace_unsafe, workspace_required=True)

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
    command.set_defaults(
        handler=_handle_workspace_unsafe, workspace_required=True)
    compare.set_defaults(workspace_required=False)

    command = commands.add_parser("test", help=_HELP["test"])
    command.set_defaults(handler=_handle_test, workspace_required=False)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    action = getattr(args, "action", None)
    label = f"{args.command} {action}" if action else args.command
    if args.workspace_required:
        if args.workspace_id is None:
            parser.error(
                f"{label}: --workspace ID is required before the command")
        args.selection = _resolve_selection(args.workspace_id)
    elif args.workspace_id is not None:
        parser.error(
            f"{label}: --workspace is not accepted for this "
            "workspace-free action")
    handler: Callable[[argparse.Namespace], Any] = args.handler
    return int(handler(args) or 0)
