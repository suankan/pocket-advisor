#!/usr/bin/env python3
"""Pocket Advisor's single executable entrypoint.

The argparse surface, pipeline, and cold retrieval live in :mod:`modules.cli`.
This file supplies a narrow adapter for maintenance and daemon commands still
implemented by the frozen ``scripts/`` tree. New modules never import legacy
modules.
"""
import os
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parent
VENV = ROOT / "venv"
VENV_PYTHON = VENV / "bin" / "python"

if VENV_PYTHON.is_file() and Path(sys.prefix).resolve() != VENV.resolve():
    os.execv(
        str(VENV_PYTHON),
        [str(VENV_PYTHON), str(ROOT / "pocket-advisor.py"), *sys.argv[1:]],
    )


def _enable_frozen_modules() -> None:
    scripts_path = str(ROOT / "scripts")
    if scripts_path not in sys.path:
        sys.path.insert(0, scripts_path)


def legacy_dispatch(args) -> int:
    """Route transitional commands to frozen pure-function modules."""
    _enable_frozen_modules()
    if args.command == "fetch-model":
        import fetch_model
        return int(fetch_model.run() or 0)
    if args.command == "daemon":
        import query_daemon
        if args.action == "serve":
            return int(query_daemon.serve(idle_sec=args.idle_sec) or 0)
        if args.action == "status":
            return int(query_daemon.cmd_status() or 0)
        return int(query_daemon.cmd_stop() or 0)
    if args.command == "wipe":
        import wipe
        handler = {
            "list": wipe.cmd_list,
            "index": wipe.cmd_index,
            "state": wipe.cmd_state,
        }[args.action]
        return int(handler(args) or 0)
    if args.command == "blob-index":
        import blob_index
        handler = {
            "list-sources": blob_index.cmd_list_sources,
            "lookup": blob_index.cmd_lookup,
        }[args.action]
        return int(handler(args) or 0)
    if args.command == "verify":
        import verify_integrity
        return int(verify_integrity.run() or 0)
    if args.command == "accuracy":
        import search_accuracy_test as accuracy
        handler = {
            "run": accuracy.cmd_run,
            "compare": accuracy.cmd_compare,
            "list": accuracy.cmd_list,
        }[args.action]
        return int(handler(args) or 0)
    raise RuntimeError(
        f"command {args.command!r} has no frozen implementation")


def main() -> int:
    from modules.cli import main as cli_main

    return cli_main(legacy_dispatch=legacy_dispatch)


if __name__ == "__main__":
    sys.exit(main())
