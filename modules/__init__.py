"""Pocket Advisor engine — full rewrite
(`docs/workspace-parsing-design.md`).

Package layout:

    config.py      typed Config + cache layout (paths only, no I/O)
    workspace.py   workspace-config.yaml v2 registry
    database.py    SQLite schema + connections (fresh schema, no migrations)
    domain.py      domain dataclasses / enums
    custody.py     sha256 + write-and-verify primitives
    review.py      review queue / ingestion_log flagging
    progress.py    terminal progress reporting
    statement_parsers.py  typed bank-statement layout parsers
    pipeline/      one Stage class per pipeline stage
    cli.py         the ONLY argument parsing in the repo
    tests/         standalone self-test scripts

Nothing in this package imports from the frozen scripts/ tree.
"""
