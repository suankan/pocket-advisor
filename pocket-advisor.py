#!/usr/bin/env python3
"""Pocket Advisor's single executable entrypoint.

The argparse surface and every workspace-scoped operation live in
:mod:`modules.cli`; the retired implementation has no runtime adapter.
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
