"""R-04: extract structured transaction-like rows from item text into SQLite.

Heuristic: lines with a date-like token and a money amount. Not a full bank
parser — enough for sums/joins with per-row citations to item_id.
Spec sketch: docs/specs/structured-transactions.md

    venv/bin/python scripts/extract_transactions.py
"""
from __future__ import annotations

import re
import sys
from pathlib import Path

import config
import db
from utils_log import now_iso

# 2024-01-15 or 15/01/2024-ish + amount
_LINE_RE = re.compile(
    r"(?P<date>\d{4}-\d{2}-\d{2}|\d{1,2}[/-]\d{1,2}[/-]\d{2,4})"
    r".{0,80}?"
    r"(?P<amount>-?\$?\d{1,3}(?:,\d{3})*(?:\.\d{2})?)",
    re.IGNORECASE,
)


def _parse_amount(s: str) -> float | None:
    s = s.replace("$", "").replace(",", "").strip()
    try:
        return float(s)
    except ValueError:
        return None


def extract_from_text(text: str, item_id: int, collection_id: str | None,
                      conn) -> int:
    n = 0
    for i, line in enumerate(text.splitlines()):
        m = _LINE_RE.search(line)
        if not m:
            continue
        amt = _parse_amount(m.group("amount"))
        if amt is None:
            continue
        conn.execute(
            """INSERT INTO transactions (
                   item_id, collection_id, txn_date, amount, currency,
                   description, row_index, raw_line, extracted_at)
               VALUES (?,?,?,?,?,?,?,?,?)""",
            (item_id, collection_id, m.group("date"), amt, "AUD",
             line.strip()[:500], i, line.strip()[:500], now_iso()),
        )
        n += 1
    return n


def run():
    conn = db.connect()
    db.migrate(conn)
    # Clear and rebuild for idempotent re-run (simple approach at this scale)
    conn.execute("DELETE FROM transactions")
    rows = conn.execute(
        """SELECT i.id, i.body_text_path, m.collection_id
           FROM items i
           LEFT JOIN item_memberships m ON m.item_id = i.id
           WHERE i.body_text_path IS NOT NULL
           GROUP BY i.id"""
    ).fetchall()
    total = 0
    for r in rows:
        path = config.PROJECT_ROOT / r["body_text_path"]
        if not path.is_file():
            continue
        text = path.read_text(encoding="utf-8", errors="replace")
        total += extract_from_text(
            text, r["id"], r["collection_id"], conn)
    conn.commit()
    n = conn.execute("SELECT COUNT(*) c FROM transactions").fetchone()["c"]
    conn.close()
    print(f"extract_transactions: {total} rows from scan; table has {n}")
    return 0


if __name__ == "__main__":
    sys.exit(run())
