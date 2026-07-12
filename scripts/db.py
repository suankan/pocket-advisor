"""SQLite schema and connection helpers.

Usage: python db.py init
"""
import sqlite3
import sys

import config

BASE_SCHEMA = """
CREATE TABLE IF NOT EXISTS emails (
    id                  INTEGER PRIMARY KEY,
    message_id          TEXT UNIQUE NOT NULL,
    date_utc            TEXT,
    date_raw            TEXT,
    from_name           TEXT,
    from_addr           TEXT,
    to_addrs            TEXT,
    cc_addrs            TEXT,
    subject             TEXT,
    subject_normalized  TEXT,
    in_reply_to         TEXT,
    references_raw      TEXT,
    thread_id           INTEGER REFERENCES threads(id),
    thread_link_method  TEXT,
    is_privileged       INTEGER NOT NULL DEFAULT 0,
    privilege_override  INTEGER,
    body_text_path      TEXT,
    body_source         TEXT,
    charset_detected    TEXT,
    has_parse_issue     INTEGER NOT NULL DEFAULT 0,
    ingested_at         TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS email_files (
    id              INTEGER PRIMARY KEY,
    email_id        INTEGER NOT NULL REFERENCES emails(id),
    source_path     TEXT UNIQUE NOT NULL,
    source_folder   TEXT NOT NULL,
    sha256          TEXT NOT NULL,
    file_size_bytes INTEGER,
    ingested_at     TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS attachments (
    id                    INTEGER PRIMARY KEY,
    email_id              INTEGER NOT NULL REFERENCES emails(id),
    parent_attachment_id  INTEGER REFERENCES attachments(id),
    filename              TEXT,
    filename_raw          TEXT,
    content_type          TEXT,
    size_bytes            INTEGER,
    sha256                TEXT NOT NULL,
    extracted_copy_path   TEXT,
    extracted_copy_sha256 TEXT,
    extraction_method     TEXT,
    extracted_text_path   TEXT,
    ocr_confidence        REAL,
    ocr_flagged_low_conf  INTEGER NOT NULL DEFAULT 0,
    is_skipped            INTEGER NOT NULL DEFAULT 0,
    skip_reason           TEXT,
    processed_at          TEXT
);

CREATE TABLE IF NOT EXISTS threads (
    id                     INTEGER PRIMARY KEY,
    representative_subject TEXT,
    first_date             TEXT,
    last_date              TEXT,
    email_count            INTEGER DEFAULT 0
);

CREATE TABLE IF NOT EXISTS chunks (
    id               INTEGER PRIMARY KEY,
    source_type      TEXT NOT NULL,
    email_id         INTEGER NOT NULL REFERENCES emails(id),
    attachment_id    INTEGER REFERENCES attachments(id),
    chunk_index      INTEGER NOT NULL,
    text             TEXT NOT NULL,
    char_start       INTEGER,
    char_end         INTEGER,
    embedded_at      TEXT,
    translit_shadow  TEXT
);

CREATE TABLE IF NOT EXISTS documents (
    id                    INTEGER PRIMARY KEY,
    email_id              INTEGER UNIQUE NOT NULL REFERENCES emails(id),
    source_path           TEXT UNIQUE NOT NULL,
    source_folder         TEXT NOT NULL,
    filename              TEXT NOT NULL,
    sha256                TEXT NOT NULL,
    size_bytes            INTEGER,
    extracted_copy_path   TEXT,
    extracted_copy_sha256 TEXT,
    extraction_method     TEXT,
    extracted_text_path   TEXT,
    ocr_confidence        REAL,
    ocr_flagged_low_conf  INTEGER NOT NULL DEFAULT 0,
    is_skipped            INTEGER NOT NULL DEFAULT 0,
    skip_reason           TEXT,
    doc_date              TEXT,
    doc_date_source       TEXT,
    doc_date_detail       TEXT,
    doc_date_raw          TEXT,
    has_parse_issue       INTEGER NOT NULL DEFAULT 0,
    ingested_at           TEXT NOT NULL,
    processed_at          TEXT
);

CREATE INDEX IF NOT EXISTS idx_documents_email ON documents(email_id);
CREATE INDEX IF NOT EXISTS idx_documents_sha ON documents(sha256);

CREATE TABLE IF NOT EXISTS ingestion_log (
    id          INTEGER PRIMARY KEY,
    file_path   TEXT,
    stage       TEXT,
    severity    TEXT,
    message     TEXT,
    occurred_at TEXT,
    resolved    INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_emails_date ON emails(date_utc);
CREATE INDEX IF NOT EXISTS idx_emails_thread ON emails(thread_id);
CREATE INDEX IF NOT EXISTS idx_emails_privileged ON emails(is_privileged);
CREATE INDEX IF NOT EXISTS idx_attachments_email ON attachments(email_id);
CREATE INDEX IF NOT EXISTS idx_chunks_email ON chunks(email_id);
"""

# chunks_fts is 2-column (text + translit_shadow, see
# docs/specs/transliteration.md) so it lives outside BASE_SCHEMA:
# CREATE VIRTUAL TABLE IF NOT EXISTS is a no-op on an already-existing
# table even with a different column count, so a real column-set change
# needs the DROP+recreate migration in _ensure_chunks_fts_shadow_column.
FTS_SCHEMA = """
CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(
    text,
    translit_shadow,
    content='chunks',
    content_rowid='id'
);

CREATE TRIGGER IF NOT EXISTS chunks_ai AFTER INSERT ON chunks BEGIN
    INSERT INTO chunks_fts(rowid, text, translit_shadow)
    VALUES (new.id, new.text, new.translit_shadow);
END;
CREATE TRIGGER IF NOT EXISTS chunks_ad AFTER DELETE ON chunks BEGIN
    INSERT INTO chunks_fts(chunks_fts, rowid, text, translit_shadow)
    VALUES ('delete', old.id, old.text, old.translit_shadow);
END;
CREATE TRIGGER IF NOT EXISTS chunks_au AFTER UPDATE ON chunks BEGIN
    INSERT INTO chunks_fts(chunks_fts, rowid, text, translit_shadow)
    VALUES ('delete', old.id, old.text, old.translit_shadow);
    INSERT INTO chunks_fts(rowid, text, translit_shadow)
    VALUES (new.id, new.text, new.translit_shadow);
END;
"""


def connect() -> sqlite3.Connection:
    conn = sqlite3.connect(config.DB_PATH)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA foreign_keys = ON")
    return conn


def ensure_column(conn, table, column, ddl):
    """Guarded ALTER TABLE: SQLite errors on a repeated ADD COLUMN and
    init() re-runs on every ingest invocation, so check first."""
    cols = {r["name"] for r in conn.execute(f"PRAGMA table_info({table})")}
    if column not in cols:
        conn.execute(f"ALTER TABLE {table} ADD COLUMN {ddl}")


def _ensure_chunks_fts_shadow_column(conn):
    """chunks_fts must be a 2-column (text, translit_shadow) FTS5
    table. If it already exists with only 1 column (pre-transliteration
    databases), DROP + recreate + rebuild from chunks — the only way to
    change an FTS5 virtual table's column set."""
    row = conn.execute(
        "SELECT sql FROM sqlite_master WHERE type='table' AND name='chunks_fts'"
    ).fetchone()
    if row and "translit_shadow" in row["sql"]:
        return
    if row:
        conn.executescript("""
            DROP TRIGGER IF EXISTS chunks_ai;
            DROP TRIGGER IF EXISTS chunks_ad;
            DROP TRIGGER IF EXISTS chunks_au;
            DROP TABLE IF EXISTS chunks_fts;
        """)
    conn.executescript(FTS_SCHEMA)
    conn.execute("INSERT INTO chunks_fts(chunks_fts) VALUES ('rebuild')")


def migrate(conn):
    """Apply schema + guarded column additions. Idempotent and silent —
    safe to call from any entrypoint (ingest stages, query.py)."""
    conn.executescript(BASE_SCHEMA)
    ensure_column(conn, "emails", "source_kind",
                  "source_kind TEXT NOT NULL DEFAULT 'email'")
    ensure_column(conn, "chunks", "translit_shadow", "translit_shadow TEXT")
    _ensure_chunks_fts_shadow_column(conn)
    conn.commit()


def init():
    config.OUTPUT_DIR.mkdir(parents=True, exist_ok=True)
    conn = connect()
    migrate(conn)
    conn.close()
    print(f"Schema initialized at {config.DB_PATH}")


def log_issue(conn, file_path, stage, severity, message):
    from datetime import datetime, timezone
    conn.execute(
        "INSERT INTO ingestion_log (file_path, stage, severity, message, occurred_at)"
        " VALUES (?, ?, ?, ?, ?)",
        (str(file_path), stage, severity, message,
         datetime.now(timezone.utc).isoformat()),
    )


if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1] == "init":
        init()
    else:
        print(__doc__)
