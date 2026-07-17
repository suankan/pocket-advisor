"""Structured issue flagging: ingestion_log rows + the human review CSV.

One ReviewLog instance per pipeline run, shared by all stages, so
logging behavior cannot drift between them.
"""
import csv
import sqlite3
from datetime import datetime, timezone
from pathlib import Path

_CSV_HEADER = ["occurred_at", "stage", "severity", "file_path", "message"]


def now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


class ReviewLog:
    def __init__(self, conn: sqlite3.Connection, csv_path: Path):
        self._conn = conn
        self._csv_path = csv_path

    def flag(self, path: object, stage: str, severity: str,
             message: str) -> None:
        """Record one issue in ingestion_log AND the review-queue CSV."""
        occurred = now_iso()
        self._conn.execute(
            "INSERT INTO ingestion_log"
            " (file_path, stage, severity, message, occurred_at)"
            " VALUES (?, ?, ?, ?, ?)",
            (str(path), stage, severity, message, occurred))
        self._csv_path.parent.mkdir(parents=True, exist_ok=True)
        is_new = not self._csv_path.exists()
        with open(self._csv_path, "a", newline="") as fh:
            writer = csv.writer(fh)
            if is_new:
                writer.writerow(_CSV_HEADER)
            writer.writerow([occurred, stage, severity, str(path), message])
