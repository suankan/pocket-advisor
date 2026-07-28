#!/usr/bin/env python3
"""Pocket Advisor's single executable entrypoint.

The argparse surface and every workspace-scoped operation live in
:mod:`modules.cli`; the retired implementation has no runtime adapter.

Invoke via ``uv run pocket-advisor.py [args]``.
"""
import sys


def main() -> int:
    from v2.modules.cli import main as cli_main

    return cli_main()


if __name__ == "__main__":
    sys.exit(main())
