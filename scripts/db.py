"""SQLite schema and connection helpers.

Usage: python db.py init
"""
import sqlite3
import sys

import config

BASE_SCHEMA = """
-- Schema B (docs/specs/schema-items-membership.md): items + memberships.
-- Schema A pathless identity preserved: UNIQUE (collection_id, sha256).

CREATE TABLE IF NOT EXISTS items (
    id                  INTEGER PRIMARY KEY,
    item_kind           TEXT NOT NULL DEFAULT 'email',
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

CREATE TABLE IF NOT EXISTS item_memberships (
    id              INTEGER PRIMARY KEY,
    item_id         INTEGER NOT NULL REFERENCES items(id),
    workspace_id    TEXT,
    collection_id   TEXT,
    source_folder   TEXT NOT NULL DEFAULT '',
    filename        TEXT NOT NULL DEFAULT '',
    sha256          TEXT NOT NULL,
    file_size_bytes INTEGER,
    membership_kind TEXT NOT NULL DEFAULT 'email',
    ingested_at     TEXT NOT NULL,
    UNIQUE (collection_id, sha256)
);

CREATE TABLE IF NOT EXISTS item_file_meta (
    item_id               INTEGER PRIMARY KEY REFERENCES items(id),
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
    processed_at          TEXT
);

CREATE TABLE IF NOT EXISTS attachments (
    id                    INTEGER PRIMARY KEY,
    item_id               INTEGER NOT NULL REFERENCES items(id),
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
    item_id          INTEGER NOT NULL REFERENCES items(id),
    attachment_id    INTEGER REFERENCES attachments(id),
    chunk_index      INTEGER NOT NULL,
    text             TEXT NOT NULL,
    char_start       INTEGER,
    char_end         INTEGER,
    embedded_at      TEXT,
    translit_shadow  TEXT
);

CREATE TABLE IF NOT EXISTS page_images (
    id                    INTEGER PRIMARY KEY,
    item_id               INTEGER NOT NULL REFERENCES items(id),
    source_kind           TEXT NOT NULL,
    attachment_id         INTEGER REFERENCES attachments(id),
    page_number           INTEGER NOT NULL,
    image_path            TEXT NOT NULL,
    sha256                TEXT NOT NULL,
    page_text_method      TEXT,
    ocr_text              TEXT,
    ocr_confidence        REAL,
    ocr_flagged_low_conf  INTEGER NOT NULL DEFAULT 0,
    rasterized_at         TEXT NOT NULL,
    img_embedded_at       TEXT,
    UNIQUE (source_kind, attachment_id, item_id, page_number)
);

CREATE TABLE IF NOT EXISTS transactions (
    id              INTEGER PRIMARY KEY,
    item_id         INTEGER NOT NULL REFERENCES items(id),
    collection_id   TEXT,
    txn_date        TEXT,
    amount          REAL,
    currency        TEXT DEFAULT 'AUD',
    description     TEXT,
    account_hint    TEXT,
    row_index       INTEGER,
    source_page     INTEGER,
    raw_line        TEXT,
    extracted_at    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_transactions_item ON transactions(item_id);
CREATE INDEX IF NOT EXISTS idx_transactions_date ON transactions(txn_date);

CREATE TABLE IF NOT EXISTS ingestion_log (
    id          INTEGER PRIMARY KEY,
    file_path   TEXT,
    stage       TEXT,
    severity    TEXT,
    message     TEXT,
    occurred_at TEXT,
    resolved    INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_items_date ON items(date_utc);
CREATE INDEX IF NOT EXISTS idx_items_thread ON items(thread_id);
CREATE INDEX IF NOT EXISTS idx_items_privileged ON items(is_privileged);
CREATE INDEX IF NOT EXISTS idx_items_kind ON items(item_kind);
CREATE INDEX IF NOT EXISTS idx_memberships_item ON item_memberships(item_id);
CREATE INDEX IF NOT EXISTS idx_memberships_collection ON item_memberships(collection_id);
CREATE INDEX IF NOT EXISTS idx_attachments_item ON attachments(item_id);
CREATE INDEX IF NOT EXISTS idx_chunks_item ON chunks(item_id);
CREATE INDEX IF NOT EXISTS idx_page_images_item ON page_images(item_id);

CREATE TABLE IF NOT EXISTS source_blob_index (
    workspace_id          TEXT,
    source_id             TEXT NOT NULL,
    sha256                TEXT NOT NULL,
    relpath_within_source TEXT NOT NULL,
    size_bytes            INTEGER,
    mtime_ns              INTEGER,
    indexed_at            TEXT NOT NULL,
    PRIMARY KEY (source_id, sha256)
);
CREATE INDEX IF NOT EXISTS idx_source_blob_source
    ON source_blob_index(source_id);
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


def _table_has_column(conn, table, column):
    return any(r["name"] == column
               for r in conn.execute(f"PRAGMA table_info({table})"))


def _migrate_pathless_evidence(conn):
    """Drop filesystem paths as identity; keep (source_id, sha256) after Phase A.

    Idempotent: only rewrites tables that still have source_path.
    """
    if _table_has_column(conn, "email_files", "source_path"):
        conn.executescript("""
            CREATE TABLE email_files_new (
                id              INTEGER PRIMARY KEY,
                email_id        INTEGER NOT NULL REFERENCES emails(id),
                workspace_id    TEXT,
                source_id       TEXT,
                source_folder   TEXT NOT NULL DEFAULT '',
                sha256          TEXT NOT NULL,
                file_size_bytes INTEGER,
                ingested_at     TEXT NOT NULL,
                UNIQUE (source_id, sha256)
            );
            INSERT OR IGNORE INTO email_files_new
                (id, email_id, workspace_id, source_id, source_folder,
                 sha256, file_size_bytes, ingested_at)
            SELECT id, email_id, workspace_id, source_id,
                   COALESCE(source_id, source_folder, ''),
                   sha256, file_size_bytes, ingested_at
            FROM email_files;
            DROP TABLE email_files;
            ALTER TABLE email_files_new RENAME TO email_files;
        """)
    if _table_has_column(conn, "documents", "source_path"):
        conn.executescript("""
            CREATE TABLE documents_new (
                id                    INTEGER PRIMARY KEY,
                email_id              INTEGER NOT NULL REFERENCES emails(id),
                workspace_id          TEXT,
                source_id             TEXT,
                source_folder         TEXT NOT NULL DEFAULT '',
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
                processed_at          TEXT,
                UNIQUE (source_id, sha256)
            );
            INSERT OR IGNORE INTO documents_new
                (id, email_id, workspace_id, source_id, source_folder, filename,
                 sha256, size_bytes, extracted_copy_path, extracted_copy_sha256,
                 extraction_method, extracted_text_path, ocr_confidence,
                 ocr_flagged_low_conf, is_skipped, skip_reason, doc_date,
                 doc_date_source, doc_date_detail, doc_date_raw, has_parse_issue,
                 ingested_at, processed_at)
            SELECT id, email_id, workspace_id, source_id,
                   COALESCE(source_id, source_folder, ''), filename,
                   sha256, size_bytes, extracted_copy_path, extracted_copy_sha256,
                   extraction_method, extracted_text_path, ocr_confidence,
                   ocr_flagged_low_conf, is_skipped, skip_reason, doc_date,
                   doc_date_source, doc_date_detail, doc_date_raw, has_parse_issue,
                   ingested_at, processed_at
            FROM documents;
            DROP TABLE documents;
            ALTER TABLE documents_new RENAME TO documents;
            CREATE INDEX IF NOT EXISTS idx_documents_email ON documents(email_id);
            CREATE INDEX IF NOT EXISTS idx_documents_sha ON documents(sha256);
        """)


def _table_sql(conn, table: str) -> str:
    row = conn.execute(
        "SELECT sql FROM sqlite_master WHERE type='table' AND name=?",
        (table,),
    ).fetchone()
    return (row["sql"] or "") if row else ""


def _unique_column_sets(conn, table: str) -> list[list[str]]:
    """Return column lists for each unique index/constraint on table."""
    out = []
    for idx in conn.execute(f"PRAGMA index_list({table})"):
        if not idx["unique"]:
            continue
        cols = [r["name"] for r in conn.execute(
            f"PRAGMA index_info('{idx['name']}')")]
        if cols:
            out.append(cols)
    return out


def _migrate_collection_identity_phase_a(conn):
    """Phase A (schema-items-membership): custody key = (source_id, sha256).

    - Drop workspace_id from UNIQUE / PRIMARY KEY on membership + blob index.
    - Allow multiple documents rows per email_id (multi-collection membership).
    Idempotent: no-op when constraints already match.
    """
    # --- email_files ---
    if _table_has_column(conn, "email_files", "id"):
        uq = _unique_column_sets(conn, "email_files")
        if ["source_id", "sha256"] not in uq:
            conn.executescript("""
                CREATE TABLE email_files_pa (
                    id              INTEGER PRIMARY KEY,
                    email_id        INTEGER NOT NULL REFERENCES emails(id),
                    workspace_id    TEXT,
                    source_id       TEXT,
                    source_folder   TEXT NOT NULL DEFAULT '',
                    sha256          TEXT NOT NULL,
                    file_size_bytes INTEGER,
                    ingested_at     TEXT NOT NULL,
                    UNIQUE (source_id, sha256)
                );
                INSERT OR IGNORE INTO email_files_pa
                    (id, email_id, workspace_id, source_id, source_folder,
                     sha256, file_size_bytes, ingested_at)
                SELECT id, email_id, workspace_id, source_id, source_folder,
                       sha256, file_size_bytes, ingested_at
                FROM email_files;
                DROP TABLE email_files;
                ALTER TABLE email_files_pa RENAME TO email_files;
            """)

    # --- documents ---
    if _table_has_column(conn, "documents", "id"):
        uq = _unique_column_sets(conn, "documents")
        needs = (
            ["email_id"] in uq
            or ["workspace_id", "source_id", "sha256"] in uq
            or ["source_id", "sha256"] not in uq
        )
        if needs:
            conn.executescript("""
                CREATE TABLE documents_pa (
                    id                    INTEGER PRIMARY KEY,
                    email_id              INTEGER NOT NULL REFERENCES emails(id),
                    workspace_id          TEXT,
                    source_id             TEXT,
                    source_folder         TEXT NOT NULL DEFAULT '',
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
                    processed_at          TEXT,
                    UNIQUE (source_id, sha256)
                );
                INSERT OR IGNORE INTO documents_pa
                    (id, email_id, workspace_id, source_id, source_folder, filename,
                     sha256, size_bytes, extracted_copy_path, extracted_copy_sha256,
                     extraction_method, extracted_text_path, ocr_confidence,
                     ocr_flagged_low_conf, is_skipped, skip_reason, doc_date,
                     doc_date_source, doc_date_detail, doc_date_raw, has_parse_issue,
                     ingested_at, processed_at)
                SELECT id, email_id, workspace_id, source_id, source_folder, filename,
                       sha256, size_bytes, extracted_copy_path, extracted_copy_sha256,
                       extraction_method, extracted_text_path, ocr_confidence,
                       ocr_flagged_low_conf, is_skipped, skip_reason, doc_date,
                       doc_date_source, doc_date_detail, doc_date_raw, has_parse_issue,
                       ingested_at, processed_at
                FROM documents;
                DROP TABLE documents;
                ALTER TABLE documents_pa RENAME TO documents;
                CREATE INDEX IF NOT EXISTS idx_documents_email ON documents(email_id);
                CREATE INDEX IF NOT EXISTS idx_documents_sha ON documents(sha256);
            """)

    # --- source_blob_index ---
    if _table_has_column(conn, "source_blob_index", "sha256"):
        uq = _unique_column_sets(conn, "source_blob_index")
        # PK shows up as unique index
        if ["source_id", "sha256"] not in uq or [
            "workspace_id", "source_id", "sha256"
        ] in uq:
            flat = " ".join(_table_sql(conn, "source_blob_index").split())
            if "PRIMARY KEY (source_id, sha256)" not in flat:
                conn.executescript("""
                    CREATE TABLE source_blob_index_pa (
                        workspace_id          TEXT,
                        source_id             TEXT NOT NULL,
                        sha256                TEXT NOT NULL,
                        relpath_within_source TEXT NOT NULL,
                        size_bytes            INTEGER,
                        mtime_ns              INTEGER,
                        indexed_at            TEXT NOT NULL,
                        PRIMARY KEY (source_id, sha256)
                    );
                    INSERT OR IGNORE INTO source_blob_index_pa
                        (workspace_id, source_id, sha256, relpath_within_source,
                         size_bytes, mtime_ns, indexed_at)
                    SELECT workspace_id, source_id, sha256, relpath_within_source,
                           size_bytes, mtime_ns, indexed_at
                    FROM source_blob_index;
                    DROP TABLE source_blob_index;
                    ALTER TABLE source_blob_index_pa RENAME TO source_blob_index;
                    CREATE INDEX IF NOT EXISTS idx_source_blob_source
                        ON source_blob_index(source_id);
                """)


def _table_exists(conn, name: str) -> bool:
    row = conn.execute(
        "SELECT 1 FROM sqlite_master WHERE type='table' AND name=?",
        (name,)).fetchone()
    return row is not None


def _migrate_schema_b_items_memberships(conn):
    """Schema B: emails→items, email_files∪documents→item_memberships,
    documents extract cols→item_file_meta, email_id→item_id on children.

    Preserves primary key ids so existing chunk/attachment FKs stay valid.
    Idempotent: no-op when emails is gone and items exists.
    """
    if not _table_exists(conn, "emails"):
        # Finish partial leftovers if any
        if _table_exists(conn, "attachments_sb"):
            conn.execute("DROP TABLE IF EXISTS attachments")
            conn.execute("ALTER TABLE attachments_sb RENAME TO attachments")
        return

    # Table rewrites with self-FKs / parent FKs need FK checks off.
    conn.execute("PRAGMA foreign_keys=OFF")

    conn.executescript("""
        CREATE TABLE IF NOT EXISTS items (
            id                  INTEGER PRIMARY KEY,
            item_kind           TEXT NOT NULL DEFAULT 'email',
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
            thread_id           INTEGER,
            thread_link_method  TEXT,
            is_privileged       INTEGER NOT NULL DEFAULT 0,
            privilege_override  INTEGER,
            body_text_path      TEXT,
            body_source         TEXT,
            charset_detected    TEXT,
            has_parse_issue     INTEGER NOT NULL DEFAULT 0,
            ingested_at         TEXT NOT NULL
        );
        INSERT OR IGNORE INTO items (
            id, item_kind, message_id, date_utc, date_raw, from_name, from_addr,
            to_addrs, cc_addrs, subject, subject_normalized, in_reply_to,
            references_raw, thread_id, thread_link_method, is_privileged,
            privilege_override, body_text_path, body_source, charset_detected,
            has_parse_issue, ingested_at
        )
        SELECT id,
               CASE WHEN COALESCE(source_kind, 'email') = 'document'
                    THEN 'file' ELSE COALESCE(source_kind, 'email') END,
               message_id, date_utc, date_raw, from_name, from_addr,
               to_addrs, cc_addrs, subject, subject_normalized, in_reply_to,
               references_raw, thread_id, thread_link_method, is_privileged,
               privilege_override, body_text_path, body_source, charset_detected,
               has_parse_issue, ingested_at
        FROM emails;
    """)

    # memberships from email_files
    if _table_exists(conn, "email_files"):
        conn.executescript("""
            CREATE TABLE IF NOT EXISTS item_memberships (
                id              INTEGER PRIMARY KEY,
                item_id         INTEGER NOT NULL,
                workspace_id    TEXT,
                collection_id   TEXT,
                source_folder   TEXT NOT NULL DEFAULT '',
                filename        TEXT NOT NULL DEFAULT '',
                sha256          TEXT NOT NULL,
                file_size_bytes INTEGER,
                membership_kind TEXT NOT NULL DEFAULT 'email',
                ingested_at     TEXT NOT NULL,
                UNIQUE (collection_id, sha256)
            );
            INSERT OR IGNORE INTO item_memberships (
                item_id, workspace_id, collection_id, source_folder, filename,
                sha256, file_size_bytes, membership_kind, ingested_at
            )
            SELECT email_id, workspace_id, source_id,
                   COALESCE(source_folder, ''), '',
                   sha256, file_size_bytes, 'email', ingested_at
            FROM email_files;
        """)

    # memberships + file meta from documents
    if _table_exists(conn, "documents"):
        conn.executescript("""
            CREATE TABLE IF NOT EXISTS item_memberships (
                id              INTEGER PRIMARY KEY,
                item_id         INTEGER NOT NULL,
                workspace_id    TEXT,
                collection_id   TEXT,
                source_folder   TEXT NOT NULL DEFAULT '',
                filename        TEXT NOT NULL DEFAULT '',
                sha256          TEXT NOT NULL,
                file_size_bytes INTEGER,
                membership_kind TEXT NOT NULL DEFAULT 'email',
                ingested_at     TEXT NOT NULL,
                UNIQUE (collection_id, sha256)
            );
            INSERT OR IGNORE INTO item_memberships (
                item_id, workspace_id, collection_id, source_folder, filename,
                sha256, file_size_bytes, membership_kind, ingested_at
            )
            SELECT email_id, workspace_id, source_id,
                   COALESCE(source_folder, ''), COALESCE(filename, ''),
                   sha256, size_bytes, 'file', ingested_at
            FROM documents;

            CREATE TABLE IF NOT EXISTS item_file_meta (
                item_id               INTEGER PRIMARY KEY,
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
                processed_at          TEXT
            );
            INSERT OR IGNORE INTO item_file_meta (
                item_id, extracted_copy_path, extracted_copy_sha256,
                extraction_method, extracted_text_path, ocr_confidence,
                ocr_flagged_low_conf, is_skipped, skip_reason,
                doc_date, doc_date_source, doc_date_detail, doc_date_raw,
                has_parse_issue, processed_at
            )
            SELECT email_id, extracted_copy_path, extracted_copy_sha256,
                   extraction_method, extracted_text_path, ocr_confidence,
                   ocr_flagged_low_conf, is_skipped, skip_reason,
                   doc_date, doc_date_source, doc_date_detail, doc_date_raw,
                   has_parse_issue, processed_at
            FROM documents
            GROUP BY email_id;
        """)

    # attachments / chunks: rewrite email_id → item_id
    if _table_exists(conn, "attachments") and _table_has_column(conn, "attachments", "email_id"):
        conn.executescript("""
            CREATE TABLE attachments_sb (
                id                    INTEGER PRIMARY KEY,
                item_id               INTEGER NOT NULL,
                parent_attachment_id  INTEGER,
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
            INSERT INTO attachments_sb
                (id, item_id, parent_attachment_id, filename, filename_raw,
                 content_type, size_bytes, sha256, extracted_copy_path,
                 extracted_copy_sha256, extraction_method, extracted_text_path,
                 ocr_confidence, ocr_flagged_low_conf, is_skipped, skip_reason,
                 processed_at)
            SELECT id, email_id, parent_attachment_id, filename, filename_raw,
                   content_type, size_bytes, sha256, extracted_copy_path,
                   extracted_copy_sha256, extraction_method, extracted_text_path,
                   ocr_confidence, ocr_flagged_low_conf, is_skipped, skip_reason,
                   processed_at
            FROM attachments;
            DROP TABLE attachments;
            ALTER TABLE attachments_sb RENAME TO attachments;
            CREATE INDEX IF NOT EXISTS idx_attachments_item ON attachments(item_id);
        """)

    if _table_exists(conn, "chunks") and _table_has_column(conn, "chunks", "email_id"):
        # Drop FTS first — depends on chunks
        conn.executescript("""
            DROP TRIGGER IF EXISTS chunks_ai;
            DROP TRIGGER IF EXISTS chunks_ad;
            DROP TRIGGER IF EXISTS chunks_au;
            DROP TABLE IF EXISTS chunks_fts;
            CREATE TABLE chunks_sb (
                id               INTEGER PRIMARY KEY,
                source_type      TEXT NOT NULL,
                item_id          INTEGER NOT NULL,
                attachment_id    INTEGER,
                chunk_index      INTEGER NOT NULL,
                text             TEXT NOT NULL,
                char_start       INTEGER,
                char_end         INTEGER,
                embedded_at      TEXT,
                translit_shadow  TEXT
            );
            INSERT INTO chunks_sb
                (id, source_type, item_id, attachment_id, chunk_index, text,
                 char_start, char_end, embedded_at, translit_shadow)
            SELECT id, source_type, email_id, attachment_id, chunk_index, text,
                   char_start, char_end, embedded_at, translit_shadow
            FROM chunks;
            DROP TABLE chunks;
            ALTER TABLE chunks_sb RENAME TO chunks;
            CREATE INDEX IF NOT EXISTS idx_chunks_item ON chunks(item_id);
        """)

    # Drop legacy tables
    conn.executescript("""
        DROP TABLE IF EXISTS documents;
        DROP TABLE IF EXISTS email_files;
        DROP TABLE IF EXISTS emails;
        DROP TABLE IF EXISTS attachments_sb;
        DROP TABLE IF EXISTS chunks_sb;
    """)
    conn.execute("PRAGMA foreign_keys=ON")


def migrate(conn):
    """Apply schema + guarded column additions. Idempotent and silent —
    safe to call from any entrypoint (ingest stages, query.py)."""
    # Legacy path: prepare old tables then convert to Schema B.
    if _table_exists(conn, "emails"):
        ensure_column(conn, "emails", "source_kind",
                      "source_kind TEXT NOT NULL DEFAULT 'email'")
        if _table_exists(conn, "chunks"):
            ensure_column(conn, "chunks", "translit_shadow", "translit_shadow TEXT")
        if _table_exists(conn, "email_files"):
            ensure_column(conn, "email_files", "workspace_id", "workspace_id TEXT")
            ensure_column(conn, "email_files", "source_id", "source_id TEXT")
        if _table_exists(conn, "documents"):
            ensure_column(conn, "documents", "workspace_id", "workspace_id TEXT")
            ensure_column(conn, "documents", "source_id", "source_id TEXT")
        _migrate_pathless_evidence(conn)
        _migrate_collection_identity_phase_a(conn)
        _migrate_schema_b_items_memberships(conn)

    conn.executescript(BASE_SCHEMA)
    ensure_column(conn, "chunks", "translit_shadow", "translit_shadow TEXT")
    ensure_column(conn, "item_memberships", "workspace_id", "workspace_id TEXT")
    ensure_column(conn, "item_memberships", "collection_id", "collection_id TEXT")
    ensure_column(conn, "item_memberships", "filename",
                  "filename TEXT NOT NULL DEFAULT ''")
    ensure_column(conn, "item_memberships", "membership_kind",
                  "membership_kind TEXT NOT NULL DEFAULT 'email'")
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
