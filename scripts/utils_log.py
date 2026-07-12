"""Shared ingestion logging: structured ingestion_log entries plus the
human review-queue CSV. Used by every ingestion path (parse_eml,
ingest_documents) so logging behavior cannot drift between them.
"""
import csv
from datetime import datetime, timezone

import config
import db


def now_iso():
    return datetime.now(timezone.utc).isoformat()


def review_queue_append(row):
    config.LOGS_DIR.mkdir(parents=True, exist_ok=True)
    new = not config.REVIEW_QUEUE_CSV.exists()
    with open(config.REVIEW_QUEUE_CSV, "a", newline="") as f:
        w = csv.writer(f)
        if new:
            w.writerow(["occurred_at", "stage", "severity", "file_path", "message"])
        w.writerow(row)


def flag(conn, path, stage, severity, message):
    db.log_issue(conn, path, stage, severity, message)
    review_queue_append([now_iso(), stage, severity, str(path), message])
