"""R-04 transaction extractor unit test (no live corpus).

    venv/bin/python scripts/test_transactions.py
"""
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import config
import db
import extract_transactions


def main():
    tmp = Path(tempfile.mkdtemp(prefix="pa_txn_"))
    try:
        config.PROJECT_ROOT = tmp
        config.WORKSPACES_DIR = tmp / "workspaces"
        config.STATE_DIR = tmp / "workspaces" / ".state"
        config.OUTPUT_DIR = config.STATE_DIR
        config.DB_PATH = config.STATE_DIR / "t.db"
        config.STATE_DIR.mkdir(parents=True)
        db.init()
        conn = db.connect()
        db.migrate(conn)
        text_dir = tmp / "text"
        text_dir.mkdir()
        body = text_dir / "1.txt"
        body.write_text(
            "Statement\n"
            "2024-01-15  GROCERY STORE  $12.50\n"
            "2024-01-16  TRANSFER IN  1,000.00\n"
            "noise line without amount\n",
            encoding="utf-8",
        )
        conn.execute(
            """INSERT INTO items (id, item_kind, message_id, subject,
               body_text_path, has_parse_issue, is_privileged, ingested_at)
               VALUES (1, 'file', '<t@x>', 'stmt', ?, 0, 0, '2024-01-01')""",
            (str(body.relative_to(tmp)),),
        )
        n = extract_transactions.extract_from_text(
            body.read_text(encoding="utf-8"), 1, "bank", conn)
        conn.commit()
        c = conn.execute("SELECT COUNT(*) c FROM transactions").fetchone()["c"]
        conn.close()
        assert n >= 2 and c >= 2, (n, c)
        print("OK extract_transactions unit")
        return 0
    finally:
        import shutil
        shutil.rmtree(tmp, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
