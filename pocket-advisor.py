#!/usr/bin/env python3
"""Pocket Advisor's single executable entrypoint.

The argparse surface and all workspace-safe operations live in
:mod:`modules.cli`. Frozen operational commands fail closed until their native
workspace-scoped ports land. New modules never import legacy modules.
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


def main() -> int:
    from modules.cli import main as cli_main

    return cli_main()


if __name__ == "__main__":
    sys.exit(main())
