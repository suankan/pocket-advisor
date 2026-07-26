"""Pocket Advisor's sole argparse surface and pipeline orchestrator."""
import argparse
import subprocess
import sys
import time
import uuid
from collections.abc import Callable
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from modules.config import Config
from modules.database import Database
from modules.domain import StageStats
from modules.logs import get_log, setup_logging
from modules.ocr import request_interrupt
from modules.pipeline.base import PipelineContext, Stage
from modules.review import ReviewLog
from modules.workspace import Registry, Workspace


ROOT = Path(__file__).resolve().parent.parent
# Ordered pipeline stages (CLI orchestration only). A named stage runs this
# prefix through itself so prerequisites are always satisfied.
PIPELINE_STAGES = (
    "discover",
    "emails",
    "pdfs",
    "thread",
    "summaries",
    "embed",
    "transactions",
)
INGEST_STAGES = ("all",) + PIPELINE_STAGES
@dataclass(frozen=True, slots=True)
class RuntimeSelection:
    """Validated workspace selection, resolved before any command effects."""

    config: Config
    registry: Registry
    workspace: Workspace


_HELP = {
    "db": "init — create the fresh SQLite schema",
    "ingest": ("all | discover | emails | pdfs | thread | summaries | embed"
               " | transactions | report [--last | PATH]"),
    "transactions": "report — statement integrity and reconciliation report",
    "query": "hybrid leaf/thread retrieval (warm daemon or cold fallback)",
    "daemon": "serve | status | stop — workspace-local warm retrieval",
    "wipe": "list | index | state — confirmed derived-state deletion",
    "blob-index": "list-sources | lookup — indexed original resolution",
    "verify": "full integrity, SQLite, FTS, artifact, and vector verification",
    "accuracy": ("generate | run | compare [--last N] | list — "
                 "retrieval expectation testing"),
    "test": "run every modules/tests/test_*.py self-test (workspace-free)",
}
_GROUPS = (
    ("setup", ("db",)),
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
        "Workspace-free:  pocket-advisor.py test",
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
    """Lazy stage imports keep help and maintenance startup light."""
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


def _stage_prefix(stage: str) -> tuple[str, ...]:
    """Return discover..stage inclusive. Named ingest runs this full prefix."""
    try:
        index = PIPELINE_STAGES.index(stage)
    except ValueError as exc:
        raise ValueError(f"unknown pipeline stage: {stage}") from exc
    return PIPELINE_STAGES[: index + 1]


def _execute_stage(
        ctx: PipelineContext, name: str, *, force_transactions: bool = False,
) -> StageStats:
    stage_class = _stage_class(name)
    if name == "transactions":
        return stage_class(ctx, force=force_transactions).execute()
    return stage_class(ctx).execute()


def _utc_now() -> datetime:
    return datetime.now(timezone.utc)


def _stage_failure_reason(exc: BaseException) -> str:
    """Return aggregate-safe failure context for a persisted run record."""
    name = type(exc).__name__
    from modules.statement_parsers import ParserConflict
    if not isinstance(exc, ParserConflict):
        return name
    # ParserConflict messages start with a structural category before the
    # first colon. Values and raw-row detail remain out of aggregate records.
    category = str(exc).partition(":")[0].strip()
    return f"{name}: {category}" if category else name


def _finalize_ingest_report(
        ctx: PipelineContext,
        *,
        started_at: str,
        ended_at: str,
        pipeline_seconds: float,
        stages: list[Any],
        start_log_id: int,
        failed_stage: str | None,
        clock: Callable[[], float],
) -> None:
    """Build, persist, and render the required full-ingest report."""
    from modules.ingest_report import (build_report, format_report,
                                       persist_report)

    report_started = clock()
    report = build_report(
        ctx,
        # Straight off the process-scoped facade: no parameter threading,
        # and the report cannot drift from the log it points at.
        run_id=ctx.log.run_id,
        started_at=started_at,
        ended_at=ended_at,
        pipeline_seconds=pipeline_seconds,
        report_seconds=0.0,
        stages=stages,
        start_log_id=start_log_id,
        failed_stage=failed_stage,
    )
    # Prepare the renderer once so format-time failures are part of the
    # required report contract. File persistence and terminal I/O themselves
    # are excluded to avoid recursively changing the serialized duration.
    format_report(report, None)
    report.report_seconds = round(clock() - report_started, 6)
    path = persist_report(report, ctx.config)
    # One record for the whole block, not thirty: the file mirrors what the
    # operator saw, at the same granularity (logging.md D2).
    ctx.log.interactive(format_report(report, path),
                        status=report.status,
                        pipeline_seconds=report.pipeline_seconds,
                        report_path=str(path))


def run_ingest(
        stage: str,
        selection: RuntimeSelection,
        *,
        force_transactions: bool = False,
        clock: Callable[[], float] = time.monotonic,
        utc_now: Callable[[], datetime] = _utc_now,
) -> int:
    """Execute one named stage or the complete ordered pipeline."""
    pipeline_started = clock()
    started_at = utc_now().isoformat()
    try:
        ctx = _open_context(selection)
    except BaseException as exc:
        if stage == "all":
            elapsed = clock() - pipeline_started
            print(
                "\nINGEST INCOMPLETE — workspace "
                f"{selection.workspace.id} — pipeline {elapsed:.1f}s\n\n"
                "This run\n"
                f"  database       failed     {elapsed:.1f}s   "
                f"{type(exc).__name__}\n\n"
                "Run report: unavailable (database did not open safely)")
        raise
    try:
        if stage != "all":
            # Named stage: run every prerequisite through the target
            # (discover … stage). Gates for embed/transactions apply only to
            # `ingest all`; an explicit named stage always executes its chain.
            from modules.dispatch import cancel_all
            from modules.embedding.dispatch import drain_leftover
            try:
                for name in _stage_prefix(stage):
                    _execute_stage(
                        ctx, name,
                        force_transactions=force_transactions
                        and name == "transactions")
            except BaseException:
                # Queued readiness embeds become durable pending gaps;
                # exit stays prompt.
                cancel_all()
                raise
            # A prefix ending before `embed` settles its in-flight
            # readiness dispatches so their vectors become durable.
            drain_leftover(ctx)
            return 0

        from modules.ingest_report import STAGE_ORDER, StageRun
        from modules.telemetry import HOT_STAGE_NAMES

        stages: list[StageRun] = []
        start_log_id = int(ctx.conn.execute(
            "SELECT coalesce(max(id), 0) FROM ingestion_log").fetchone()[0])

        def execute(name: str) -> None:
            stage_started = clock()
            if name in HOT_STAGE_NAMES:
                ctx.telemetry.mark_entered(name)
            try:
                stats = _execute_stage(ctx, name)
            except BaseException as exc:
                request_interrupt()
                from modules.dispatch import cancel_all
                cancel_all()
                stages.append(StageRun(
                    name=name,
                    outcome="failed",
                    duration_seconds=round(clock() - stage_started, 6),
                    stats={},
                    reason=_stage_failure_reason(exc),
                ))
                raise
            if name in HOT_STAGE_NAMES:
                # Seals partial as measured; a stage-recorded deliberate
                # not_applicable gate is preserved.
                ctx.telemetry.mark_measured(name)
            stages.append(StageRun(
                name=name,
                outcome="completed",
                duration_seconds=round(clock() - stage_started, 6),
                stats=dict(stats.counts),
            ))

        def skip(name: str, reason: str) -> None:
            if name in HOT_STAGE_NAMES:
                ctx.telemetry.mark_not_applicable(name)
            stages.append(StageRun(
                name=name,
                outcome="skipped",
                duration_seconds=None,
                stats={},
                reason=reason,
            ))

        # summaries always runs: its staleness maintenance must see every
        # ingest even when generation is disabled (the stage gates the
        # generative pass on ingestion.summarize_threads itself).
        try:
            for name in (
                    "discover", "emails", "pdfs", "thread", "summaries"):
                execute(name)
            if ctx.config.embed_text:
                execute("embed")
            else:
                reason = "ingestion.embed_text=false"
                print(f"embed: skipped ({reason})")
                skip("embed", reason)
            has_bank_collections = any(
                collection.is_bank_transactions
                for collection in ctx.workspace.collections)
            from modules.pipeline.transactions import has_transaction_state
            if has_bank_collections or has_transaction_state(ctx):
                execute("transactions")
            else:
                reason = "no mounted bank-transactions collections"
                print(f"transactions: skipped ({reason})")
                skip("transactions", reason)
        except BaseException:
            failed_stage = stages[-1].name
            completed_names = {item.name for item in stages}
            for name in STAGE_ORDER:
                if name not in completed_names:
                    stages.append(StageRun(
                        name=name,
                        outcome="not_run",
                        duration_seconds=None,
                        stats={},
                        reason=f"pipeline stopped at {failed_stage}",
                    ))
            try:
                ctx.conn.rollback()
            except Exception:
                pass
            ended_at = utc_now().isoformat()
            pipeline_seconds = clock() - pipeline_started
            try:
                _finalize_ingest_report(
                    ctx,
                    started_at=started_at,
                    ended_at=ended_at,
                    pipeline_seconds=pipeline_seconds,
                    stages=stages,
                    start_log_id=start_log_id,
                    failed_stage=failed_stage,
                    clock=clock,
                )
            except BaseException as report_exc:
                print(
                    "ingest report failed while preserving the original "
                    f"pipeline error: {type(report_exc).__name__}: "
                    f"{report_exc}")
            raise

        ended_at = utc_now().isoformat()
        pipeline_seconds = clock() - pipeline_started
        try:
            _finalize_ingest_report(
                ctx,
                started_at=started_at,
                ended_at=ended_at,
                pipeline_seconds=pipeline_seconds,
                stages=stages,
                start_log_id=start_log_id,
                failed_stage=None,
                clock=clock,
            )
        except Exception as exc:
            print(
                "\nINGEST REPORT FAILED — stages completed and their state "
                "may be committed — "
                f"{type(exc).__name__}: {exc}")
            return 1
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


def _handle_ingest(args: argparse.Namespace) -> int:
    if args.force and args.stage != "transactions":
        raise SystemExit(
            "ingest: --force applies only to 'ingest transactions'")
    if args.stage == "report":
        return _show_ingest_report(args)
    if args.record is not None or args.last:
        raise SystemExit(
            "ingest: --last / a record path apply only to 'ingest report'")
    return run_ingest(
        args.stage, args.selection, force_transactions=args.force)


def _show_ingest_report(args: argparse.Namespace) -> int:
    """Render one persisted ingest run record (default: the latest)."""
    from modules.ingest_report import (format_report, latest_report_path,
                                       load_report)

    selection: RuntimeSelection = args.selection
    if args.record is not None and args.last:
        raise SystemExit(
            "ingest report: pass either --last or a record path, not both")
    if args.record is not None:
        path = Path(args.record)
        if not path.is_file():
            candidate = selection.config.project_root / args.record
            if not candidate.is_file():
                raise SystemExit(
                    f"ingest report: no such record: {args.record}")
            path = candidate
    else:
        path = latest_report_path(selection.config)
        if path is None:
            raise SystemExit(
                "ingest report: no saved run reports under "
                f"{selection.config.logs_dir / 'ingest-runs'}")
    print(format_report(load_report(path), path))
    return 0


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

    selection: RuntimeSelection = args.selection
    use_daemon = not args.no_daemon and (
        args.require_daemon or selection.config.daemon_auto)
    if use_daemon:
        from modules.daemon import daemon_request
        payload = {
            "op": "search",
            "question": args.question,
            "top_k": args.top_k,
            "after": args.after,
            "before": args.before,
            "thread": args.thread,
            "purpose": args.purpose,
            "no_thread_context": args.no_thread_context,
        }
        try:
            response = daemon_request(
                selection.config, payload, timeout=600.0)
        except (OSError, ValueError, ConnectionError) as exc:
            if args.require_daemon:
                raise SystemExit(
                    f"query: required daemon is unavailable: {exc}") from exc
        else:
            if not response.get("ok"):
                raise SystemExit(
                    f"query daemon: {response.get('error', 'search failed')}")
            print("query: via workspace daemon (warm)", file=sys.stderr)
            format_results(response["result"], as_json=args.json)
            return 0

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


def _handle_daemon(args: argparse.Namespace) -> int:
    from modules import daemon

    selection: RuntimeSelection = args.selection
    if args.action == "status":
        return daemon.status(selection.config)
    if args.action == "stop":
        return daemon.stop(selection.config)
    ctx = _open_context(selection)
    try:
        return daemon.serve(ctx, args.idle_sec)
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


def _handle_accuracy(args: argparse.Namespace) -> int:
    from modules import accuracy

    selection: RuntimeSelection = args.selection
    paths = accuracy.suite_paths(selection.config)

    if args.action == "list":
        print(accuracy.format_list(paths))
        return 0

    if args.action == "compare":
        if args.last < 1:
            raise SystemExit("accuracy compare: --last must be >= 1")
        files = accuracy.result_files(paths)
        if len(files) < 2:
            raise SystemExit(
                f"accuracy compare: need at least 2 results under "
                f"{paths.results_dir}, found {len(files)}")
        window = files[-(args.last + 1):]
        results = [accuracy.load_result(path) for path in window]
        print(accuracy.format_compare(
            results, [path.name for path in window]))
        return 0

    ctx = _open_context(selection)
    try:
        if args.action == "generate":
            target = paths.expectations_dir / accuracy.GENERATED_EXPECTATIONS_NAME
            limit = getattr(args, "limit", None)
            target, stats = accuracy.generate_expectations(
                ctx, target, args.force, limit=limit)
            print(
                f"accuracy: generated {stats.generated} questions "
                f"(considered={stats.considered}, "
                f"skipped_empty={stats.skipped_empty}, "
                f"rejected={stats.rejected}) "
                f"via {stats.model} prompt-v{stats.prompt_version}\n"
                f"  -> {target}\n"
                "Run 'accuracy run' to score retrieval against them.")
            return 0
        files = accuracy.expectation_files(paths, args.expectations)
        entries = accuracy.load_expectations(files)
        top_k = args.top_k or ctx.config.default_top_k
        if top_k <= 0:
            raise SystemExit("accuracy run: --top-k must be positive")
        print(f"accuracy: {len(entries)} expectations from "
              f"{', '.join(path.name for path in files)} — loading models…")
        result = accuracy.run_expectations(
            ctx, entries, files, top_k=top_k, label=args.label)
        record = accuracy.persist_result(result, paths)
        print(accuracy.format_run(result, record))
        return 1 if result["aggregates"]["miss"] \
            or result["aggregates"]["invalid"] else 0
    finally:
        ctx.conn.close()


def _handle_wipe_state(args: argparse.Namespace) -> int:
    from modules.daemon import stop as stop_daemon
    from modules.wipe import wipe_state

    selection: RuntimeSelection = args.selection

    def before_delete() -> None:
        if stop_daemon(selection.config) != 0:
            raise SystemExit("wipe state: could not stop the workspace daemon")

    return wipe_state(
        selection.config,
        selection.registry,
        selection.workspace,
        yes=args.yes,
        before_delete=before_delete,
    )


def _handle_wipe(args: argparse.Namespace) -> int:
    from modules.daemon import stop as stop_daemon
    from modules.wipe import format_index_list, wipe_indexes

    selection: RuntimeSelection = args.selection
    if args.action == "list":
        print(format_index_list(selection.config))
        return 0

    def before_active_delete() -> None:
        if stop_daemon(selection.config) != 0:
            raise SystemExit("wipe index: could not stop the workspace daemon")

    return wipe_indexes(
        selection.config,
        slug=args.text,
        all_inactive=args.all_inactive,
        yes=args.yes,
        force=args.force,
        before_active_delete=before_active_delete,
    )


def _handle_blob_index(args: argparse.Namespace) -> int:
    from modules.maintenance import format_sources, lookup_blob

    ctx = _open_context(args.selection)
    try:
        if args.action == "list-sources":
            print(format_sources(ctx))
            return 0
        path = lookup_blob(
            ctx, args.source, args.sha256,
            verify_hash=not args.no_verify)
        print(path)
        return 0
    finally:
        ctx.conn.close()


def _handle_verify(args: argparse.Namespace) -> int:
    from modules.maintenance import format_verification, verify_workspace

    ctx = _open_context(args.selection)
    try:
        report = verify_workspace(ctx)
        print(format_verification(report))
        return 0 if report.ok else 1
    finally:
        ctx.conn.close()


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="pocket-advisor.py",
        description="Pocket Advisor — local content ingestion and retrieval",
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

    command = commands.add_parser(
        "ingest",
        help=_HELP["ingest"],
        description=(
            "Run all pipeline stages or one named stage together with every "
            "prerequisite stage in order (e.g. 'ingest pdfs' runs discover, "
            "emails, then pdfs). Stages are idempotent. 'ingest report' "
            "re-renders a saved full-ingest run record instead of running "
            "anything (default: the workspace's latest record)."),
    )
    command.add_argument(
        "stage",
        nargs="?",
        choices=INGEST_STAGES + ("report",),
        default="all",
        help="pipeline stage (default: all), or 'report' to display a "
             "saved run record",
    )
    command.add_argument(
        "record",
        nargs="?",
        metavar="PATH",
        help="saved run record to display (ingest report only)",
    )
    command.add_argument(
        "--last",
        action="store_true",
        help="display the workspace's latest saved run record "
             "(ingest report only; this is also the default)",
    )
    command.add_argument(
        "--force",
        action="store_true",
        help="force a full rebuild (ingest transactions only)",
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
    daemon_mode = command.add_mutually_exclusive_group()
    daemon_mode.add_argument("--no-daemon", action="store_true")
    daemon_mode.add_argument("--require-daemon", action="store_true")
    command.set_defaults(handler=_handle_query, workspace_required=True)

    command = commands.add_parser("daemon", help=_HELP["daemon"])
    actions = command.add_subparsers(
        dest="action", metavar="action", required=True)
    serve = actions.add_parser("serve", help="run in foreground")
    serve.add_argument("--idle-sec", type=int, default=None)
    actions.add_parser("status", help="show process and loaded-index status")
    actions.add_parser("stop", help="request clean socket shutdown")
    command.set_defaults(
        handler=_handle_daemon, idle_sec=None, workspace_required=True)

    command = commands.add_parser("wipe", help=_HELP["wipe"])
    actions = command.add_subparsers(
        dest="action", metavar="action", required=True)
    actions.add_parser("list", help="list selected workspace vector caches")
    wipe_index = actions.add_parser(
        "index", help="delete one or all inactive vector caches")
    wipe_index.add_argument("--text", metavar="SLUG")
    wipe_index.add_argument("--all-inactive", action="store_true")
    wipe_index.add_argument("--yes", action="store_true")
    wipe_index.add_argument("--force", action="store_true")
    wipe_state = actions.add_parser(
        "state", help="delete the selected workspace's full derived state")
    wipe_state.add_argument("--yes", action="store_true")
    command.set_defaults(handler=_handle_wipe, workspace_required=True)
    wipe_state.set_defaults(handler=_handle_wipe_state)

    command = commands.add_parser("blob-index", help=_HELP["blob-index"])
    actions = command.add_subparsers(
        dest="action", metavar="action", required=True)
    actions.add_parser(
        "list-sources", help="show mounted collection roots and index counts")
    lookup = actions.add_parser(
        "lookup", help="resolve and verify one indexed original")
    lookup.add_argument("--source", "-s", required=True)
    lookup.add_argument("--sha256", required=True)
    lookup.add_argument("--no-verify", action="store_true")
    command.set_defaults(handler=_handle_blob_index, workspace_required=True)

    command = commands.add_parser("verify", help=_HELP["verify"])
    command.set_defaults(handler=_handle_verify, workspace_required=True)

    command = commands.add_parser("accuracy", help=_HELP["accuracy"])
    actions = command.add_subparsers(
        dest="action", metavar="action", required=True)
    generate = actions.add_parser(
        "generate",
        help="synthesize expectation questions from body/PDF text (local MLX)")
    generate.add_argument("--force", action="store_true")
    generate.add_argument(
        "--limit", type=int, default=None, metavar="N",
        help="generate at most N candidates after deterministic ordering")
    run = actions.add_parser(
        "run", help="run the expectation set(s); write a JSON result record")
    run.add_argument("--expectations", type=Path, default=None,
                     help="one expectation file (default: every *.yaml in "
                          "the workspace's expectations directory)")
    run.add_argument("--label", default="run")
    run.add_argument("--top-k", type=int, default=None)
    compare = actions.add_parser(
        "compare", help="compare the newest result with previous runs")
    compare.add_argument("--last", type=int, default=1, metavar="N",
                         help="how many previous results to compare "
                              "against (default 1)")
    actions.add_parser("list", help="list saved result records")
    command.set_defaults(
        handler=_handle_accuracy, workspace_required=True)

    command = commands.add_parser("test", help=_HELP["test"])
    command.set_defaults(handler=_handle_test, workspace_required=False)
    return parser


def _dispatch(handler: Callable[[argparse.Namespace], Any],
              args: argparse.Namespace) -> int:
    try:
        return int(handler(args) or 0)
    except KeyboardInterrupt:
        # Ctrl+C already unwinds the pipeline cleanly via the interrupt flag;
        # suppress the traceback and exit with the conventional SIGINT code.
        # Queued readiness embeds are abandoned as durable pending gaps so
        # exit is prompt — the next `ingest embed` fills them.
        try:
            from modules.dispatch import cancel_all
            cancel_all()
        except Exception:
            pass
        # Recorded, not printed: an interrupted run is exactly the one whose
        # log needs to say why it stopped. Terminal routing goes through any
        # live progress bar, so no redraw line is left half-drawn.
        get_log().error("KeyboardInterrupt — interrupted")
        return 130


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

    selection: RuntimeSelection | None = getattr(args, "selection", None)
    if selection is None:
        # Workspace-free action: execution logs are workspace-scoped, so
        # there is nowhere to write one. The null facade keeps terminal
        # output working (modules/logs.py D6).
        return _dispatch(handler, args)

    run_id = str(uuid.uuid4())
    with setup_logging(selection.config, run_id=run_id) as log:
        # Banner and footer on stderr, once each: the operator gets the
        # token to query by, without 36 characters of UUID on every line
        # (logging.md D7). stderr keeps piped stdout clean.
        print(f"pocket-advisor: run {run_id} — "
              f"workspace {selection.workspace.id}", file=sys.stderr)
        try:
            return _dispatch(handler, args)
        finally:
            # `finally`, so the runs worth correlating — the failed and the
            # interrupted ones — still say where their log is.
            print(f"Run log:    {log.path} (run {run_id})", file=sys.stderr)
