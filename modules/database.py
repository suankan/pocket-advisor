"""SQLite schema and connections — fresh Schema C, NO migration chain.

Ingestion design v2 (`docs/ingestion/ingestion-design-v2.md`) replaces the
`item_kind`-conflated `items` table and the email-owned attachment cache
with a normalized content-addressed content graph: unique `emails`, unique
`documents`, and explicit source/attachment occurrence tables. This ships
as a wipe + full re-ingest, so this module carries no legacy migrations.
A database created by an earlier engine generation is detected and refused
with a pointer to `wipe state` — never silently half-upgraded.

Schema continuity: threads / chunks / chunks_fts and the transactions
family keep their established names; `chunks`/`statements` now reference
`email_id`/`document_id` instead of `item_id`.
"""
import sqlite3
from pathlib import Path

SCHEMA = """
CREATE TABLE IF NOT EXISTS workspace_metadata (
    singleton    INTEGER PRIMARY KEY CHECK (singleton = 1),
    workspace_id TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS ingestion_candidates (
    id            INTEGER PRIMARY KEY,
    workspace_id  TEXT,
    collection_id TEXT NOT NULL,
    relpath       TEXT NOT NULL,     -- first-seen provenance, not identity
    sha256        TEXT NOT NULL,
    size_bytes    INTEGER,
    document_type TEXT NOT NULL
                  CHECK (document_type IN ('email', 'pdf', 'other')),
    status        TEXT NOT NULL DEFAULT 'candidate'
                  CHECK (status IN ('candidate', 'ingested',
                                    'skipped', 'error')),
    discovered_at TEXT NOT NULL,
    UNIQUE (collection_id, sha256)
);

-- One row per unique raw email byte stream (top-level or attached).
-- Identity is the raw-email sha256, never Message-ID (not globally unique).
CREATE TABLE IF NOT EXISTS emails (
    id                    INTEGER PRIMARY KEY,
    sha256                TEXT NOT NULL UNIQUE,
    message_id            TEXT,       -- native header, NOT unique: retained
                                       -- and reviewable on collision
    reply_parent_email_id INTEGER REFERENCES emails(id),  -- conversation edge
    date_utc              TEXT,
    date_raw              TEXT,
    from_name             TEXT,
    from_addr             TEXT,
    to_addrs              TEXT,
    cc_addrs              TEXT,
    subject               TEXT,
    subject_normalized    TEXT,
    in_reply_to           TEXT,
    references_raw        TEXT,
    thread_id             INTEGER REFERENCES threads(id),
    thread_link_method    TEXT,
    -- email_message.txt (authored/searchable) and email_message_full.txt
    -- (lossless), both under emails/<sha256>/.
    body_text_path        TEXT,
    body_full_text_path   TEXT,
    body_quote_start      INTEGER,
    body_quote_boundary_method TEXT,
    body_compaction_method TEXT,
    body_compaction_parent_email_id INTEGER REFERENCES emails(id),
    body_compaction_removed_chars INTEGER NOT NULL DEFAULT 0,
    body_compaction_version INTEGER,
    body_source           TEXT,
    charset_detected      TEXT,
    has_parse_issue       INTEGER NOT NULL DEFAULT 0,
    ingested_at           TEXT NOT NULL
);

-- Every top-level or recursively observed source occurrence that supplies
-- an email byte stream (separates email identity from carrying paths).
CREATE TABLE IF NOT EXISTS email_sources (
    id              INTEGER PRIMARY KEY,
    email_id        INTEGER NOT NULL REFERENCES emails(id),
    workspace_id    TEXT,
    collection_id   TEXT NOT NULL,
    relpath         TEXT NOT NULL,
    file_size_bytes INTEGER,
    discovered_at   TEXT NOT NULL,
    UNIQUE (collection_id, relpath, email_id)
);

-- One row per unique retained binary object (SHA-256 within workspace).
-- PDFs, images, zip archives, and other non-email attachments; a bank
-- statement mounted directly in a collection is a document too. Absorbs
-- the former item_file_meta extraction/OCR state, now 1:1 with content
-- identity rather than per-occurrence.
CREATE TABLE IF NOT EXISTS documents (
    id                    INTEGER PRIMARY KEY,
    sha256                TEXT NOT NULL UNIQUE,
    media_kind            TEXT NOT NULL
                          CHECK (media_kind IN
                              ('pdf', 'image', 'zip', 'other')),
    content_type          TEXT,
    size_bytes            INTEGER NOT NULL,
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
    processed_at          TEXT,
    ingested_at           TEXT NOT NULL
);

-- Every native collection occurrence of a document (parallel to
-- email_sources).
CREATE TABLE IF NOT EXISTS document_sources (
    id              INTEGER PRIMARY KEY,
    document_id     INTEGER NOT NULL REFERENCES documents(id),
    workspace_id    TEXT,
    collection_id   TEXT NOT NULL,
    relpath         TEXT NOT NULL,
    file_size_bytes INTEGER,
    discovered_at   TEXT NOT NULL,
    UNIQUE (collection_id, relpath, document_id)
);

-- Pure occurrence/join row: one email-to-payload relationship. The
-- payload is either a document (document_id) or a child message/rfc822
-- email (child_email_id) — exactly one, never both, never neither.
-- ZIP members are attachment rows linked through the carrying ZIP
-- occurrence via parent_attachment_id (nesting/order preserved without
-- copying into an email-owned folder).
CREATE TABLE IF NOT EXISTS attachments (
    id                   INTEGER PRIMARY KEY,
    email_id             INTEGER NOT NULL REFERENCES emails(id),
    document_id          INTEGER REFERENCES documents(id),
    child_email_id       INTEGER REFERENCES emails(id),
    parent_attachment_id INTEGER REFERENCES attachments(id),
    filename             TEXT,
    filename_raw         TEXT,
    content_type         TEXT,
    size_bytes           INTEGER,
    ordinal              INTEGER NOT NULL DEFAULT 0,
    ingested_at          TEXT NOT NULL,
    CHECK ((document_id IS NOT NULL) + (child_email_id IS NOT NULL) = 1)
);

CREATE TABLE IF NOT EXISTS threads (
    id                     INTEGER PRIMARY KEY,
    stable_key             TEXT NOT NULL UNIQUE,
    representative_subject TEXT,
    first_date             TEXT,
    last_date              TEXT,
    item_count             INTEGER DEFAULT 0
);

CREATE TABLE IF NOT EXISTS thread_summaries (
    thread_id       INTEGER PRIMARY KEY REFERENCES threads(id)
                    ON DELETE CASCADE,
    summary_text    TEXT NOT NULL,
    source_digest   TEXT NOT NULL,
    generator_model TEXT NOT NULL,
    prompt_version  INTEGER NOT NULL,
    is_stale        INTEGER NOT NULL DEFAULT 0,
    generated_at    TEXT NOT NULL
);

-- source_type distinguishes email-body chunks from document-text chunks;
-- exactly one of email_id/document_id is set.
CREATE TABLE IF NOT EXISTS chunks (
    id               INTEGER PRIMARY KEY,
    source_type      TEXT NOT NULL,
    email_id         INTEGER REFERENCES emails(id),
    document_id      INTEGER REFERENCES documents(id),
    chunk_index      INTEGER NOT NULL,
    text             TEXT NOT NULL,
    char_start       INTEGER,
    char_end         INTEGER,
    embedded_at      TEXT,
    translit_shadow  TEXT,
    payload_shadow   TEXT,
    CHECK ((email_id IS NOT NULL) + (document_id IS NOT NULL) = 1)
);

-- R-04b structured transactions
-- (`docs_old/specs/structured-transactions-v2.md`).
-- Money is signed integer minor units everywhere; negative = egress.

CREATE TABLE IF NOT EXISTS holders (
    id           INTEGER PRIMARY KEY,
    display_name TEXT UNIQUE NOT NULL,
    notes        TEXT
);

CREATE TABLE IF NOT EXISTS accounts (
    id             INTEGER PRIMARY KEY,
    config_id      TEXT UNIQUE NOT NULL,
    bsb            TEXT,
    account_number TEXT NOT NULL,
    type           TEXT NOT NULL,
    currency       TEXT NOT NULL DEFAULT 'AUD',
    label          TEXT
);

CREATE TABLE IF NOT EXISTS account_owners (
    account_id INTEGER NOT NULL REFERENCES accounts(id),
    holder_id  INTEGER NOT NULL REFERENCES holders(id),
    UNIQUE (account_id, holder_id)
);

CREATE TABLE IF NOT EXISTS statements (
    id                    INTEGER PRIMARY KEY,
    document_id           INTEGER NOT NULL REFERENCES documents(id),
    account_id            INTEGER REFERENCES accounts(id),
    period_start          TEXT,
    period_end            TEXT,
    opening_balance_minor INTEGER,
    closing_balance_minor INTEGER,
    parser_id             TEXT,
    balance_ok            INTEGER,
    pdf_producer          TEXT,
    pdf_created           TEXT,
    pdf_modified          TEXT,
    parsed_at             TEXT,
    excluded              INTEGER NOT NULL DEFAULT 0,
    UNIQUE (document_id, account_id, period_start)
);

CREATE TABLE IF NOT EXISTS statement_assertions (
    id             INTEGER PRIMARY KEY,
    statement_id   INTEGER NOT NULL REFERENCES statements(id),
    kind           TEXT NOT NULL CHECK (kind IN (
                       'opening_balance', 'closing_balance', 'total_credits',
                       'total_debits', 'txn_count', 'carried_forward',
                       'running_balance_chain')),
    as_of_date     TEXT,
    amount_minor   INTEGER,
    count          INTEGER,
    page_no        INTEGER,
    raw_line       TEXT,
    passed         INTEGER,
    observed_minor INTEGER,
    observed_count INTEGER,
    UNIQUE (statement_id, kind, page_no)
);

CREATE TABLE IF NOT EXISTS transactions (
    id                  INTEGER PRIMARY KEY,
    statement_id        INTEGER NOT NULL REFERENCES statements(id),
    account_id          INTEGER REFERENCES accounts(id),
    txn_date            TEXT,
    value_date          TEXT,
    amount_minor        INTEGER NOT NULL,
    currency            TEXT NOT NULL DEFAULT 'AUD',
    description_raw     TEXT,
    counterparty_raw    TEXT,
    balance_after_minor INTEGER,
    page_no             INTEGER,
    row_index           INTEGER NOT NULL,
    raw_line            TEXT,
    UNIQUE (statement_id, row_index)
);

CREATE TABLE IF NOT EXISTS transfer_links (
    id                 INTEGER PRIMARY KEY,
    from_txn_id        INTEGER NOT NULL REFERENCES transactions(id),
    to_txn_id          INTEGER NOT NULL REFERENCES transactions(id),
    match_kind         TEXT NOT NULL CHECK (match_kind IN
                           ('exact', 'fee_adjusted', 'manual')),
    date_delta_days    INTEGER,
    amount_delta_minor INTEGER,
    source             TEXT NOT NULL CHECK (source IN ('auto', 'override')),
    UNIQUE (from_txn_id, to_txn_id)
);

CREATE TABLE IF NOT EXISTS ingestion_log (
    id          INTEGER PRIMARY KEY,
    file_path   TEXT,
    stage       TEXT,
    severity    TEXT,
    message     TEXT,
    occurred_at TEXT,
    resolved    INTEGER NOT NULL DEFAULT 0
);

-- sha256-to-path cache, refreshed by the discover walk.
CREATE TABLE IF NOT EXISTS source_blob_index (
    workspace_id          TEXT,
    source_id             TEXT NOT NULL,
    sha256                TEXT NOT NULL,
    relpath_within_source TEXT NOT NULL,
    size_bytes            INTEGER,
    mtime_ns              INTEGER,
    indexed_at            TEXT NOT NULL,
    PRIMARY KEY (source_id, sha256, relpath_within_source)
);

CREATE INDEX IF NOT EXISTS idx_candidates_status
    ON ingestion_candidates(status);
CREATE INDEX IF NOT EXISTS idx_candidates_type
    ON ingestion_candidates(document_type);
CREATE INDEX IF NOT EXISTS idx_emails_date ON emails(date_utc);
CREATE INDEX IF NOT EXISTS idx_emails_thread ON emails(thread_id);
CREATE INDEX IF NOT EXISTS idx_emails_reply_parent
    ON emails(reply_parent_email_id);
CREATE INDEX IF NOT EXISTS idx_emails_message_id ON emails(message_id);
CREATE INDEX IF NOT EXISTS idx_email_sources_email ON email_sources(email_id);
CREATE INDEX IF NOT EXISTS idx_email_sources_collection
    ON email_sources(collection_id);
CREATE INDEX IF NOT EXISTS idx_documents_media_kind
    ON documents(media_kind);
CREATE INDEX IF NOT EXISTS idx_document_sources_document
    ON document_sources(document_id);
CREATE INDEX IF NOT EXISTS idx_document_sources_collection
    ON document_sources(collection_id);
CREATE INDEX IF NOT EXISTS idx_attachments_email ON attachments(email_id);
CREATE INDEX IF NOT EXISTS idx_attachments_document
    ON attachments(document_id);
CREATE INDEX IF NOT EXISTS idx_attachments_child_email
    ON attachments(child_email_id);
CREATE INDEX IF NOT EXISTS idx_attachments_parent
    ON attachments(parent_attachment_id);
CREATE INDEX IF NOT EXISTS idx_chunks_email ON chunks(email_id);
CREATE INDEX IF NOT EXISTS idx_chunks_document ON chunks(document_id);
CREATE INDEX IF NOT EXISTS idx_statements_account ON statements(account_id);
CREATE INDEX IF NOT EXISTS idx_statements_document
    ON statements(document_id);
CREATE INDEX IF NOT EXISTS idx_transactions_stmt
    ON transactions(statement_id);
CREATE INDEX IF NOT EXISTS idx_transactions_acct_date
    ON transactions(account_id, txn_date);
CREATE INDEX IF NOT EXISTS idx_transactions_date ON transactions(txn_date);
CREATE INDEX IF NOT EXISTS idx_transactions_amount
    ON transactions(amount_minor);
CREATE INDEX IF NOT EXISTS idx_assertions_stmt
    ON statement_assertions(statement_id);
CREATE INDEX IF NOT EXISTS idx_source_blob_source
    ON source_blob_index(source_id);
"""

# chunks_fts is contentless-synced to chunks via triggers. `text` remains the
# pure source quote; payload_shadow carries the envelope-enriched search
# payload, while translit_shadow preserves the proper-noun fallback.
FTS_SCHEMA = """
CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(
    text,
    translit_shadow,
    payload_shadow,
    content='chunks',
    content_rowid='id'
);

CREATE TRIGGER IF NOT EXISTS chunks_ai AFTER INSERT ON chunks BEGIN
    INSERT INTO chunks_fts(rowid, text, translit_shadow, payload_shadow)
    VALUES (new.id, new.text, new.translit_shadow, new.payload_shadow);
END;
CREATE TRIGGER IF NOT EXISTS chunks_ad AFTER DELETE ON chunks BEGIN
    INSERT INTO chunks_fts(
        chunks_fts, rowid, text, translit_shadow, payload_shadow)
    VALUES ('delete', old.id, old.text, old.translit_shadow,
            old.payload_shadow);
END;
CREATE TRIGGER IF NOT EXISTS chunks_au AFTER UPDATE ON chunks BEGIN
    INSERT INTO chunks_fts(
        chunks_fts, rowid, text, translit_shadow, payload_shadow)
    VALUES ('delete', old.id, old.text, old.translit_shadow,
            old.payload_shadow);
    INSERT INTO chunks_fts(rowid, text, translit_shadow, payload_shadow)
    VALUES (new.id, new.text, new.translit_shadow, new.payload_shadow);
END;

CREATE VIRTUAL TABLE IF NOT EXISTS thread_summaries_fts USING fts5(
    summary_text,
    content='thread_summaries',
    content_rowid='thread_id'
);

CREATE TRIGGER IF NOT EXISTS thread_summaries_ai
AFTER INSERT ON thread_summaries BEGIN
    INSERT INTO thread_summaries_fts(rowid, summary_text)
    VALUES (new.thread_id, new.summary_text);
END;
CREATE TRIGGER IF NOT EXISTS thread_summaries_ad
AFTER DELETE ON thread_summaries BEGIN
    INSERT INTO thread_summaries_fts(
        thread_summaries_fts, rowid, summary_text)
    VALUES ('delete', old.thread_id, old.summary_text);
END;
CREATE TRIGGER IF NOT EXISTS thread_summaries_au
AFTER UPDATE ON thread_summaries BEGIN
    INSERT INTO thread_summaries_fts(
        thread_summaries_fts, rowid, summary_text)
    VALUES ('delete', old.thread_id, old.summary_text);
    INSERT INTO thread_summaries_fts(rowid, summary_text)
    VALUES (new.thread_id, new.summary_text);
END;
"""

# Tables/columns that only an earlier pipeline generation created. Their
# presence means the DB predates this schema and must be wiped, not
# patched — this module deliberately has no migration chain.
#
# NOTE: `emails` and `documents` are THIS schema's own table names (an
# even earlier, pre-`items` engine generation used those same names with
# an incompatible shape — `email_files`/`page_images` were its other
# markers). We cannot use table-name presence alone to detect the
# schema generation this cutover retires, because `attachments` is
# reused across both generations with a different column set. So legacy
# detection here checks: (a) the retired `items`/`item_memberships`/
# `item_file_meta` table names, and (b) an `attachments` table that
# lacks the current `document_id` column (the pre-cutover shape).
_LEGACY_TABLES = ("items", "item_memberships", "item_file_meta",
                  "email_files", "page_images")


class LegacyDatabaseError(SystemExit):
    def __init__(self, db_path: Path, workspace_id: str, marker: str):
        super().__init__(
            f"database {db_path} is not the current fresh schema "
            f"(found {marker}). This engine has no migration chain — "
            "run `./pocket-advisor.py --workspace "
            f"{workspace_id} wipe state` (confirmed; wipes only that "
            "workspace's derived state) and re-ingest from corpora.")


class WorkspaceDatabaseError(SystemExit):
    """A database is unbound, misbound, or contains foreign workspace rows."""

    def __init__(self, db_path: Path, message: str):
        super().__init__(f"database {db_path}: {message}")


class Database:
    """Connection factory + schema owner for the engine SQLite DB."""

    def __init__(self, path: Path, workspace_id: str):
        self.path = path
        self.workspace_id = workspace_id

    def connect(self) -> sqlite3.Connection:
        # timeout: wait for other writers (daemon, parallel CLI) instead
        # of failing immediately with "database is locked".
        conn = sqlite3.connect(self.path, timeout=60.0)
        conn.row_factory = sqlite3.Row
        conn.execute("PRAGMA foreign_keys = ON")
        conn.execute("PRAGMA busy_timeout = 60000")  # ms
        return conn

    def open(self) -> sqlite3.Connection:
        """Connect AND ensure the schema — the normal entry point."""
        self.path.parent.mkdir(parents=True, exist_ok=True)
        conn = self.connect()
        try:
            tables = self._tables(conn)
            is_new = not tables
            self._refuse_legacy(conn, tables)
            if not is_new:
                self._require_workspace_binding(conn)
                self._verify_workspace_rows(conn, tables)
            conn.executescript(SCHEMA)
            conn.executescript(FTS_SCHEMA)
            if is_new:
                conn.execute(
                    "INSERT INTO workspace_metadata"
                    " (singleton, workspace_id) VALUES (1, ?)",
                    (self.workspace_id,))
            self._require_workspace_binding(conn)
            self._verify_workspace_rows(conn, self._tables(conn))
            conn.commit()
            return conn
        except BaseException:
            conn.close()
            raise

    @staticmethod
    def _tables(conn: sqlite3.Connection) -> set[str]:
        return {row["name"] for row in conn.execute(
            "SELECT name FROM sqlite_master WHERE type = 'table'")}

    def _refuse_legacy(
            self, conn: sqlite3.Connection, tables: set[str]) -> None:
        for marker in _LEGACY_TABLES:
            if marker in tables:
                raise LegacyDatabaseError(
                    self.path, self.workspace_id, f"table {marker!r}")
        if tables and "workspace_metadata" not in tables:
            raise LegacyDatabaseError(
                self.path, self.workspace_id,
                "workspace binding metadata missing")
        if "attachments" in tables:
            columns = {row["name"] for row in
                       conn.execute("PRAGMA table_info(attachments)")}
            if "document_id" not in columns:
                raise LegacyDatabaseError(
                    self.path, self.workspace_id,
                    "attachments predates the content-addressed content"
                    " graph (missing document_id)")
        if "emails" in tables:
            columns = {row["name"] for row in
                       conn.execute("PRAGMA table_info(emails)")}
            required = {"sha256"}
            missing = required - columns
            if missing:
                raise LegacyDatabaseError(
                    self.path, self.workspace_id,
                    "emails missing " + ", ".join(sorted(missing)))
        if "documents" in tables:
            columns = {row["name"] for row in
                       conn.execute("PRAGMA table_info(documents)")}
            if "sha256" not in columns:
                raise LegacyDatabaseError(
                    self.path, self.workspace_id, "documents missing sha256")
        if "threads" in tables:
            columns = {row["name"] for row in
                       conn.execute("PRAGMA table_info(threads)")}
            required = {"stable_key", "item_count"}
            missing = required - columns
            if missing:
                raise LegacyDatabaseError(
                    self.path, self.workspace_id, "threads missing "
                    + ", ".join(sorted(missing)))
        if "chunks" in tables:
            columns = {row["name"] for row in
                       conn.execute("PRAGMA table_info(chunks)")}
            required = {"payload_shadow", "email_id", "document_id"}
            missing = required - columns
            if missing:
                raise LegacyDatabaseError(
                    self.path, self.workspace_id,
                    "chunks missing " + ", ".join(sorted(missing)))
        if "chunks_fts" in tables:
            columns = {row["name"] for row in
                       conn.execute("PRAGMA table_info(chunks_fts)")}
            if "payload_shadow" not in columns:
                raise LegacyDatabaseError(
                    self.path, self.workspace_id,
                    "chunks_fts missing payload_shadow")

    def _require_workspace_binding(self, conn: sqlite3.Connection) -> None:
        row = conn.execute(
            "SELECT workspace_id FROM workspace_metadata"
            " WHERE singleton = 1").fetchone()
        if row is None:
            raise WorkspaceDatabaseError(
                self.path, "workspace binding row is missing")
        bound = str(row["workspace_id"])
        if bound != self.workspace_id:
            raise WorkspaceDatabaseError(
                self.path,
                f"bound to workspace {bound!r}, not selected workspace "
                f"{self.workspace_id!r}")

    def _verify_workspace_rows(
            self, conn: sqlite3.Connection, tables: set[str]) -> None:
        for table in ("ingestion_candidates", "email_sources",
                      "document_sources", "source_blob_index"):
            if table not in tables:
                continue
            row = conn.execute(
                f"SELECT workspace_id FROM {table}"
                " WHERE workspace_id IS NOT NULL AND workspace_id != ?"
                " LIMIT 1",
                (self.workspace_id,)).fetchone()
            if row is not None:
                raise WorkspaceDatabaseError(
                    self.path,
                    f"{table} contains row for workspace "
                    f"{row['workspace_id']!r}; expected {self.workspace_id!r}")
