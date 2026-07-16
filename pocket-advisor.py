#!/usr/bin/env python3
"""Pocket Advisor — the single entrypoint for operating the RAG engine.

    ./pocket-advisor.py <command> [args...]
    ./pocket-advisor.py <command> --help

The ONLY argument parsing in the codebase lives here; everything under
scripts/ is a pure module (functions only, no argparse, no __main__).
Any interpreter may launch this — it re-execs itself under the repo
venv (the MLX stack lives there).
"""
import argparse
import os
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent

_VENV = ROOT / "venv"
_VENV_PY = _VENV / "bin" / "python"
if _VENV_PY.is_file() and Path(sys.prefix).resolve() != _VENV.resolve():
    os.execv(str(_VENV_PY), [str(_VENV_PY), str(ROOT / "pocket-advisor.py")]
             + sys.argv[1:])

sys.path.insert(0, str(ROOT / "scripts"))


# ---------------------------------------------------------------------------
# handlers — each lazily imports its module and calls plain functions

def _db(a):
    import db
    db.init()
    return 0


def _fetch_model(a):
    import fetch_model
    return fetch_model.run() or 0


def _ingest(a):
    import ingest
    return ingest.cli(a.stage, a.embed)


def _transactions(a):
    import transactions
    return transactions.cli(a.action)


def _query(a):
    import query
    return query.cli(a) or 0


def _daemon(a):
    import query_daemon
    if a.action == "serve":
        return query_daemon.serve(idle_sec=a.idle_sec) or 0
    if a.action == "status":
        return query_daemon.cmd_status()
    return query_daemon.cmd_stop()


def _wipe(a):
    import wipe
    return {"list": wipe.cmd_list, "index": wipe.cmd_index,
            "state": wipe.cmd_state}[a.action](a)


def _blob_index(a):
    import blob_index
    return {"rebuild": blob_index.cmd_rebuild,
            "list-sources": blob_index.cmd_list_sources,
            "lookup": blob_index.cmd_lookup}[a.action](a)


def _verify(a):
    import verify_integrity
    return verify_integrity.run()


def _accuracy(a):
    import search_accuracy_test as sat
    return {"run": sat.cmd_run, "compare": sat.cmd_compare,
            "list": sat.cmd_list}[a.action](a) or 0


def _test(a):
    """Run every self-test script, PASS/FAIL per file, non-zero on any
    failure. Tests stay standalone scripts (each sandboxes its own
    config/db before importing engine modules)."""
    tests = sorted((ROOT / "scripts").glob("test_*.py"))
    if not tests:
        print("pocket-advisor test: no scripts/test_*.py found")
        return 1
    failed = []
    for t in tests:
        r = subprocess.run([sys.executable, str(t)],
                           capture_output=True, text=True)
        ok = r.returncode == 0
        print(f"  [{'ok' if ok else 'FAIL'}] {t.name}")
        if not ok:
            failed.append(t.name)
            tail = (r.stdout + r.stderr).strip().splitlines()[-15:]
            for line in tail:
                print(f"      {line}")
    print(f"pocket-advisor test: {len(tests) - len(failed)}/{len(tests)} "
          f"passed" + (f" — FAILED: {', '.join(failed)}" if failed else ""))
    return 1 if failed else 0


# ---------------------------------------------------------------------------
# the one parser

_HELP = {
    "db": "init — create/upgrade the SQLite schema",
    "fetch-model": "download the configured MLX model repos (one-time, "
                   "inbound weights only)",
    "ingest": "all | parse | documents | attachments | thread; "
              "--embed text|all",
    "transactions": "parse | link | report — bank-statement tables, "
                    "transfer matching, integrity report",
    "query": "one-off retrieval query (auto-uses the daemon when live)",
    "daemon": "serve | status | stop — session-warm query daemon",
    "wipe": "list | index | state — the ONLY thing that deletes derived "
            "state (vector caches / full .state wipe)",
    "blob-index": "rebuild | list-sources | lookup — sha256->path custody "
                  "cache",
    "verify": "integrity check (run before privilege logs / exports)",
    "accuracy": "run | compare | list — golden-set search accuracy",
    "test": "run every scripts/test_*.py self-test",
}

_GROUPS = (("setup", ("db", "fetch-model")),
           ("pipeline", ("ingest", "transactions")),
           ("retrieval", ("query", "daemon")),
           ("maintenance", ("wipe", "blob-index", "verify")),
           ("quality", ("accuracy", "test")))


def _epilog() -> str:
    lines = ["commands:"]
    for group, names in _GROUPS:
        lines.append(f"  {group}:")
        for n in names:
            lines.append(f"    {n:14} {_HELP[n]}")
    lines.append("")
    lines.append("Per-command flags: pocket-advisor.py <command> --help")
    return "\n".join(lines)


def build_parser() -> argparse.ArgumentParser:
    ap = argparse.ArgumentParser(
        prog="pocket-advisor.py",
        description=__doc__.splitlines()[0],
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=_epilog(),
    )
    sub = ap.add_subparsers(dest="command", metavar="command", required=True)

    p = sub.add_parser("db", help=_HELP["db"])
    p.add_argument("action", choices=("init",),
                   help="init: create/upgrade the SQLite schema")
    p.set_defaults(func=_db)

    p = sub.add_parser("fetch-model", help=_HELP["fetch-model"])
    p.set_defaults(func=_fetch_model)

    p = sub.add_parser(
        "ingest", help=_HELP["ingest"],
        description="Ingestion orchestrator (parse / thread / embed).")
    p.add_argument("stage", nargs="?", default=None,
                   help="all | parse | documents | attachments | thread | "
                        "transactions (default: all when no --embed)")
    p.add_argument("--embed", choices=("text", "all"),
                   default=None, metavar="MODE",
                   help="embedding mode: text | all "
                        "(all respects ingestion.embed_text)")
    p.set_defaults(func=_ingest)

    p = sub.add_parser("transactions", help=_HELP["transactions"])
    p.add_argument("action", choices=("parse", "link", "report"))
    p.set_defaults(func=_transactions)

    p = sub.add_parser("query", help=_HELP["query"])
    p.add_argument("question")
    p.add_argument("--after", help="only items on/after YYYY-MM-DD")
    p.add_argument("--before", help="only items on/before YYYY-MM-DD")
    p.add_argument("--thread", type=int, help="restrict to one thread id")
    priv = p.add_mutually_exclusive_group()
    priv.add_argument(
        "--include-privileged", action="store_true", default=None,
        help="include privileged collections (default: on; see config)")
    priv.add_argument(
        "--exclude-privileged", action="store_true",
        help="exclude privileged collections (restricted retrieval pass)")
    p.add_argument("--purpose", default=None,
                   help="R-05: only search collections mounted for this "
                        "purpose tag")
    p.add_argument("--top-k", type=int, default=None,
                   help="results to return (default from config)")
    p.add_argument("--no-thread-context", action="store_true")
    p.add_argument("--json", action="store_true")
    p.add_argument("--no-daemon", action="store_true",
                   help="force cold local search (ignore running daemon)")
    p.add_argument("--require-daemon", action="store_true",
                   help="fail if the warm query daemon is not reachable")
    p.set_defaults(func=_query)

    p = sub.add_parser("daemon", help=_HELP["daemon"])
    dsub = p.add_subparsers(dest="action", metavar="action", required=True)
    d = dsub.add_parser("serve", help="run daemon in foreground")
    d.add_argument("--idle-sec", type=int, default=None,
                   help="override config idle timeout (0=never)")
    dsub.add_parser("status", help="ping running daemon")
    dsub.add_parser("stop", help="ask daemon to shut down")
    p.set_defaults(func=_daemon, idle_sec=None)

    p = sub.add_parser("wipe", help=_HELP["wipe"])
    wsub = p.add_subparsers(dest="action", metavar="action", required=True)
    wsub.add_parser("list", help="show every cached index on disk")
    w = wsub.add_parser("index",
                        help="delete a cached vector index (manual, explicit)")
    w.add_argument("--text", metavar="SLUG", help="delete this text index")
    w.add_argument("--all-inactive", action="store_true",
                   help="delete every cached index except the currently "
                        "active text index")
    w.add_argument("--yes", action="store_true",
                   help="skip confirmation prompt")
    w.add_argument("--force", action="store_true",
                   help="allow wiping the currently ACTIVE index")
    w = wsub.add_parser("state",
                        help="FULL wipe of workspaces/.state (DB + every "
                             "index) for a from-scratch re-ingest")
    w.add_argument("--yes", action="store_true",
                   help="skip confirmation prompt")
    p.set_defaults(func=_wipe)

    p = sub.add_parser("blob-index", help=_HELP["blob-index"])
    bsub = p.add_subparsers(dest="action", metavar="action", required=True)
    bsub.add_parser("rebuild", help="rebuild cache for all known sources")
    bsub.add_parser("list-sources",
                    help="show workspace_id, source_id, root")
    b = bsub.add_parser("lookup", help="resolve path for a sha256")
    b.add_argument("--workspace", "-w", required=True)
    b.add_argument("--source", "-s", required=True)
    b.add_argument("--sha256", required=True)
    b.add_argument("--no-verify", action="store_true",
                   help="skip re-hash of resolved file")
    p.set_defaults(func=_blob_index)

    p = sub.add_parser("verify", help=_HELP["verify"])
    p.set_defaults(func=_verify)

    p = sub.add_parser("accuracy", help=_HELP["accuracy"])
    asub = p.add_subparsers(dest="action", metavar="action", required=True)
    a = asub.add_parser("run",
                        help="run the golden set through query and score it")
    a.add_argument("--golden", required=True, help="path to golden set YAML")
    a.add_argument("--label", default="run")
    a.add_argument("--top-k", type=int, default=None,
                   help="results per question (default from config)")
    a.add_argument("--mode", choices=("warm", "cold"), default="warm",
                   help="warm (default): load embed/rerank once in-process. "
                        "cold: subprocess a full query per question "
                        "(CLI cold-start cost).")
    a = asub.add_parser("compare", help="compare two result JSON files")
    a.add_argument("result_a")
    a.add_argument("result_b")
    a = asub.add_parser("list", help="list past runs")
    a.add_argument("--golden", help="filter to runs of this golden set")
    p.set_defaults(func=_accuracy)

    p = sub.add_parser("test", help=_HELP["test"])
    p.set_defaults(func=_test)

    return ap


def main(argv=None) -> int:
    ns = build_parser().parse_args(argv)
    return int(ns.func(ns) or 0)


if __name__ == "__main__":
    sys.exit(main())
