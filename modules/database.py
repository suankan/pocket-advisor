"""SQLite schema and connections — fresh Schema B, NO migration chain.

The clean-break refactor (`docs/workspace-parsing-design.md`) ships with a
wipe + full re-ingest, so this module carries no legacy migrations.
A database created by the old scripts/ tree is detected and refused
with a pointer to `wipe state` — never silently half-upgraded.

Retrieval compatibility: items / chunks / chunks_fts / threads and the
transactions family keep their old names and columns (the frozen
retrieval stack keeps working); new in this schema are
`ingestion_candidates` (the Stage 1 working set) and
`items.parent_item_id` (attached-email lineage).
"""
import sqlite3
from pathlib import Path

SCHEMA = """
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

CREATE TABLE IF NOT EXISTS items (
    id                  INTEGER PRIMARY KEY,
    item_kind           TEXT NOT NULL DEFAULT 'email',
    message_id          TEXT UNIQUE NOT NULL,
    parent_item_id      INTEGER REFERENCES items(id),  -- attached-email lineage
    reply_parent_item_id INTEGER REFERENCES items(id), -- conversation edge
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
    -- body_text_path = email_body_authored.txt (searchable);
    -- body_full_text_path = email_body_full.txt (lossless). Old column
    -- names kept: the frozen retrieval stack reads them.
    body_text_path      TEXT,
    body_full_text_path TEXT,
    body_quote_start    INTEGER,
    body_quote_boundary_method TEXT,
    body_compaction_method TEXT,
    body_compaction_parent_item_id INTEGER REFERENCES items(id),
    body_compaction_removed_chars INTEGER NOT NULL DEFAULT 0,
    body_compaction_version INTEGER,
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

-- Native-PDF extraction metadata (doc dates, OCR provenance).
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
    stable_key             TEXT NOT NULL UNIQUE,
    representative_subject TEXT,
    first_date             TEXT,
    last_date              TEXT,
    email_count            INTEGER DEFAULT 0
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
    item_id               INTEGER NOT NULL REFERENCES items(id),
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
    UNIQUE (item_id, account_id, period_start)
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

-- Pure custody cache (sha256 -> path), refreshed by the discover walk.
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

CREATE INDEX IF NOT EXISTS idx_candidates_status
    ON ingestion_candidates(status);
CREATE INDEX IF NOT EXISTS idx_candidates_type
    ON ingestion_candidates(document_type);
CREATE INDEX IF NOT EXISTS idx_items_date ON items(date_utc);
CREATE INDEX IF NOT EXISTS idx_items_thread ON items(thread_id);
CREATE INDEX IF NOT EXISTS idx_items_kind ON items(item_kind);
CREATE INDEX IF NOT EXISTS idx_items_parent ON items(parent_item_id);
CREATE INDEX IF NOT EXISTS idx_items_reply_parent
    ON items(reply_parent_item_id);
CREATE INDEX IF NOT EXISTS idx_memberships_item ON item_memberships(item_id);
CREATE INDEX IF NOT EXISTS idx_memberships_collection
    ON item_memberships(collection_id);
CREATE INDEX IF NOT EXISTS idx_attachments_item ON attachments(item_id);
CREATE INDEX IF NOT EXISTS idx_chunks_item ON chunks(item_id);
CREATE INDEX IF NOT EXISTS idx_statements_account ON statements(account_id);
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

# chunks_fts is contentless-synced to chunks via triggers; 2-column
# (text + translit_shadow, `docs_old/specs/transliteration.md`).
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

# Tables/columns that only the OLD scripts/ pipeline creates. Their
# presence means the DB predates this schema and must be wiped, not
# patched — this module deliberately has no migration chain.
_LEGACY_TABLES = ("emails", "email_files", "documents", "page_images")


class LegacyDatabaseError(SystemExit):
    def __init__(self, db_path: Path, marker: str):
        super().__init__(
            f"database {db_path} is not the current fresh schema "
            f"(found {marker}). This engine has no migration chain — "
            "run `./pocket-advisor.py wipe state` (confirmed, wipes ALL "
            "derived state) and re-ingest from corpora.")


class Database:
    """Connection factory + schema owner for the engine SQLite DB."""

    def __init__(self, path: Path):
        self.path = path

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
        self._refuse_legacy(conn)
        conn.executescript(SCHEMA)
        conn.executescript(FTS_SCHEMA)
        conn.commit()
        return conn

    def _refuse_legacy(self, conn: sqlite3.Connection) -> None:
        tables = {row["name"] for row in conn.execute(
            "SELECT name FROM sqlite_master WHERE type = 'table'")}
        for marker in _LEGACY_TABLES:
            if marker in tables:
                raise LegacyDatabaseError(self.path, f"table {marker!r}")
        if "items" in tables:
            columns = {row["name"] for row in
                       conn.execute("PRAGMA table_info(items)")}
            required = {"parent_item_id", "reply_parent_item_id"}
            missing = required - columns
            if missing:
                raise LegacyDatabaseError(
                    self.path, "items missing " + ", ".join(sorted(missing)))
        if "threads" in tables:
            columns = {row["name"] for row in
                       conn.execute("PRAGMA table_info(threads)")}
            if "stable_key" not in columns:
                raise LegacyDatabaseError(
                    self.path, "threads without stable_key")
