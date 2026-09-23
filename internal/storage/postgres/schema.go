package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// schemaSQL is the Tier 2 + Tier 3 DDL. The vector dimension is interpolated
// at bootstrap from a probe of the embedding endpoint — it is never a literal
// in checked-in DDL (ingestion-design.md §4.4).
//
// The email browse tables are spliced on rather than written inline: an
// already-provisioned workspace never re-runs this DDL, so the same statements
// have to be reachable as an upgrade (applyEmailSchema) as well as at
// bootstrap. Every statement in that half is IF NOT EXISTS, so the two paths
// are the same statements run under different circumstances rather than two
// descriptions of one schema that could drift apart.
const schemaSQL = coreSchemaSQL + placementSchemaSQL + emailSchemaSQL

const coreSchemaSQL = `
CREATE EXTENSION IF NOT EXISTS vector;

-- Postgres grants CONNECT to PUBLIC on every new database by default —
-- verified directly: with this un-run, a second workspace's role could
-- connect to this one's database and list its tables (though not read them;
-- object-level grants still held). One shared cluster (deviation 34) makes
-- that reachable in a way one cluster per workspace never was, since there
-- was no server to connect to in the first place. The database owner can
-- revoke its own database's PUBLIC connect without being superuser — also
-- verified directly — so this runs here, in the same DDL the workspace's own
-- role already applies, rather than needing a separate administrative step.
--
-- The identifier-quoting placeholder below is doubled up: schemaSQL is
-- itself a fmt.Sprintf format string (for the vector dimension below), so a
-- single instance would be a Go verb, not Postgres's format() placeholder —
-- go vet catches the unescaped form as an unknown verb, and left unescaped it
-- would have mangled the REVOKE at runtime, not just failed to vet clean.
DO $$ BEGIN
    EXECUTE format('REVOKE CONNECT ON DATABASE %%I FROM PUBLIC', current_database());
END $$;

DO $$ BEGIN
    CREATE TYPE processing_status AS ENUM
        ('PENDING','PROCESSING','COMPLETED','SKIPPED','FAILED');
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

-- One workspace, one database (deviation 34): workspace_id belongs here,
-- exactly once for the whole database, and nowhere else. Every other table
-- in this schema used to repeat it on every row and filter by it on every
-- query, which is a predicate that is always true — it can never distinguish
-- one row from another *within* a database that only ever holds one
-- workspace. AssertScope reads it from here now instead of taking a distinct
-- sample of a data column, so the check remains cheap and exact even for an
-- otherwise empty workspace.
CREATE TABLE IF NOT EXISTS schema_metadata (
    id            BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    workspace_id  VARCHAR NOT NULL,
    embed_model   VARCHAR NOT NULL,
    embed_dim     INT     NOT NULL,
    truncated_dim BOOLEAN NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS documents (
    doc_id            UUID PRIMARY KEY,
    parent_doc_id     UUID REFERENCES documents(doc_id) ON DELETE CASCADE,
    processing_status processing_status NOT NULL DEFAULT 'PENDING',
    doc_type          VARCHAR NOT NULL DEFAULT '',
    mime_type         VARCHAR NOT NULL DEFAULT '',
    source_ref    TEXT    NOT NULL DEFAULT '',
    raw_sha256        VARCHAR NOT NULL DEFAULT '',
    source_filename   TEXT    NOT NULL DEFAULT '',
    -- Body prose only. RFC822 headers live in the columns below, not inline
    -- here: they are metadata, they repeat identically across every message
    -- in a thread, and keeping them out keeps this column exactly what a
    -- person wrote (§5.3).
    normalized_text   TEXT,
    email_subject     TEXT    NOT NULL DEFAULT '',
    email_from        TEXT    NOT NULL DEFAULT '',
    email_to          TEXT    NOT NULL DEFAULT '',
    email_date        TIMESTAMPTZ,
    metadata_headers  JSONB   NOT NULL DEFAULT '{}'::jsonb,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS documents_parent_idx     ON documents(parent_doc_id);

-- doc_id and raw_sha256 are deliberately independent: doc_id is an opaque,
-- randomly generated row identifier (domain.NewDocID) with no relationship
-- to the bytes it names, and raw_sha256 is the content identity idempotency
-- actually depends on. This constraint is what CreateStub upserts against
-- (ON CONFLICT (raw_sha256)) to converge two racing intake attempts, a
-- retried publish, and a re-scan of the same bytes on one row — the
-- guarantee doc_id used to provide by being derived from the hash itself,
-- now provided by this constraint instead of by doc_id's own value.
DO $$ BEGIN
    ALTER TABLE documents
        ADD CONSTRAINT documents_raw_sha256_key UNIQUE (raw_sha256);
EXCEPTION WHEN duplicate_object OR duplicate_table THEN NULL; END $$;

-- Drives the stale-PENDING reconciliation sweep (§2.2).
CREATE INDEX IF NOT EXISTS documents_pending_idx
    ON documents(updated_at) WHERE processing_status = 'PENDING';
-- Chronological ordering and date-range filters over messages (§5.3).
CREATE INDEX IF NOT EXISTS documents_email_date_idx
    ON documents(email_date DESC NULLS LAST);

-- A passage, stored once however many documents contain it.
--
-- Boilerplate repeats: a disclaimer, a letterhead, a template paragraph is its
-- own chunk in every document that carries it, and measured on a real corpus
-- one stored chunk in six was a copy of another, each carrying its own
-- embedding and its own index entries. Identity is the SHA-256 of the text
-- with whitespace runs collapsed, because extraction reflows the same
-- paragraph differently per document, so the bytes differ while the passage
-- does not. raw_sha256 plays the same content-identity role
-- documents.raw_sha256 does — enforced by the constraint below — even though
-- what it hashes is normalised chunk text, not a document's raw bytes.
--
-- chunk_id is an opaque, randomly generated row identifier (like
-- documents.doc_id), independent of raw_sha256 for the same reason doc_id is
-- independent of documents.raw_sha256: a short, indexable cross-reference
-- key, not a derivation of what it names.
--
-- Identity is scoped by embed_model: a re-embed writes a new model
-- namespace, and collapsing across namespaces would serve a vector from the
-- wrong model. Equality only — never a similarity threshold — because
-- passages that differ only in dates or amounts must remain distinct.
CREATE TABLE IF NOT EXISTS chunks (
    chunk_id     UUID PRIMARY KEY,
    embed_model  VARCHAR NOT NULL,
    raw_sha256   BYTEA   NOT NULL,
    chunk_text   TEXT    NOT NULL,
    embedding    halfvec(%[1]d),
    UNIQUE (embed_model, raw_sha256)
);

CREATE INDEX IF NOT EXISTS chunks_hnsw_idx ON chunks
    USING hnsw (embedding halfvec_cosine_ops) WITH (m = 16, ef_construction = 64);

-- Where a passage sits in one document. Offsets and ordinal are per-document
-- and stay here: the same passage occupies different byte ranges in each
-- document that contains it, so folding them onto the shared row would make a
-- citation resolve against the wrong text.
--
-- placement_id is the identity of a passage-in-a-document, which is what
-- retrieval ranks and what a packet cites. One row per placement, exactly as
-- before deduplication, so the read path sees the same candidates it always
-- did — only the text and vector behind them are now shared. chunk_id here
-- is the placement's own reference to the shared passage it points at — the
-- same name as chunks' own primary key, exactly the way doc_id names both
-- documents' primary key and every foreign key that points at it.
CREATE TABLE IF NOT EXISTS document_chunks (
    placement_id      UUID PRIMARY KEY,
    doc_id            UUID NOT NULL REFERENCES documents(doc_id) ON DELETE CASCADE,
    chunk_id          UUID NOT NULL REFERENCES chunks(chunk_id) ON DELETE RESTRICT,
    chunk_index       INT     NOT NULL,
    start_char_offset INT     NOT NULL,
    end_char_offset   INT     NOT NULL
);

CREATE INDEX IF NOT EXISTS chunks_doc_idx        ON document_chunks(doc_id);
CREATE INDEX IF NOT EXISTS chunks_chunk_idx      ON document_chunks(chunk_id);
`

// placementSchemaSQL is containment tracking and the deletion semantics that
// depend on it (ingestion-design.md §6.5). Structural apart from one seed from
// documents.parent_doc_id, which is a floor rather than the truth — see
// applyPlacementSchema.
const placementSchemaSQL = `
-- Which documents contain which, one row per containment.
--
-- documents.parent_doc_id cannot answer this and was never meant to. It is a
-- single column, and content identity is unique per workspace
-- (documents_raw_sha256_key), so an attachment carried by two emails is one
-- row whose parent points at whichever email happened to be processed first;
-- the second email's containment was discarded. That is harmless for
-- provenance — "where did this come from" wants one answer — and fatal for
-- deletion, which needs the other question: is anything still carrying this?
-- Pruning a container used to cascade through parent_doc_id and take a shared
-- attachment with it, silently stripping it from an email nobody asked to
-- remove, and leaving no record that it had ever been there.
--
-- So this table answers containment and parent_doc_id keeps answering
-- provenance. Extra rows here are safe and missing rows are not: an edge too
-- many retains a document that could have gone, an edge too few deletes one
-- that something still holds. Everything that writes here errs toward more.
CREATE TABLE IF NOT EXISTS document_placements (
    container_doc_id UUID NOT NULL REFERENCES documents(doc_id) ON DELETE CASCADE,
    contained_doc_id UUID NOT NULL REFERENCES documents(doc_id) ON DELETE CASCADE,
    first_seen_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (container_doc_id, contained_doc_id)
);

-- The reverse direction is the one pruning asks: who still contains this?
CREATE INDEX IF NOT EXISTS document_placements_contained_idx
    ON document_placements(contained_doc_id);

-- Every parent link that exists is a containment that is certainly real, so
-- it seeds the table. This is a floor, not the truth: it reproduces exactly
-- the single-parent under-counting described above, which is why pruning
-- additionally requires the backfill below to have re-derived the rest.
INSERT INTO document_placements (container_doc_id, contained_doc_id)
SELECT parent_doc_id, doc_id FROM documents WHERE parent_doc_id IS NOT NULL
ON CONFLICT DO NOTHING;

-- Deleting a container must not delete what it contains — that is the whole
-- point of the table above, and a CASCADE here would overrule it before the
-- sweep ever ran. Survivors are repointed at a container that remains.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'documents_parent_doc_id_fkey'
          AND conrelid = 'documents'::regclass
          AND confdeltype <> 'n'
    ) THEN
        ALTER TABLE documents DROP CONSTRAINT documents_parent_doc_id_fkey;
        ALTER TABLE documents
            ADD CONSTRAINT documents_parent_doc_id_fkey
            FOREIGN KEY (parent_doc_id) REFERENCES documents(doc_id) ON DELETE SET NULL;
    END IF;
END $$;

-- When the containment backfill last completed. Pruning refuses to run while
-- this is NULL: on un-backfilled data the placement table is the parent_doc_id
-- floor, and sweeping against a floor deletes shared attachments.
ALTER TABLE schema_metadata ADD COLUMN IF NOT EXISTS placements_backfilled_at TIMESTAMPTZ;
`

// emailSchemaSQL is the durable email browse model
// (ingestion-design.md §2.5). It is structural only: no data is rewritten, so
// it is safe on an existing workspace and a no-op on every run after the first.
const emailSchemaSQL = `
-- One row per email message document. documents keeps the display-form header
-- values the retrieval path already renders; this table keeps the structured
-- identities a browse query joins and sorts on, which is why sent_at exists
-- alongside documents.email_date rather than replacing it.
--
-- sent_at is the message's own Date and is NULL when it was absent or
-- unparsable; ingested_at is when this row was written. They are separate
-- because a message with no date must still be reachable, and the snapshot
-- watermark that keeps a paginated browse stable is an ingestion fact, not a
-- claim the sender made.
CREATE TABLE IF NOT EXISTS email_messages (
    doc_id              UUID PRIMARY KEY REFERENCES documents(doc_id) ON DELETE CASCADE,
    message_id          TEXT        NOT NULL DEFAULT '',
    subject_raw         TEXT        NOT NULL DEFAULT '',
    sent_at             TIMESTAMPTZ,
    ingested_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- '' | 'list' | 'auto_submitted' | 'bounce'. A closed set derived from a
    -- closed set of header rules; '' is ordinary human-authored mail.
    automated_class     VARCHAR     NOT NULL DEFAULT '',
    list_id             TEXT        NOT NULL DEFAULT '',
    parse_warnings      JSONB       NOT NULL DEFAULT '[]'::jsonb,
    parse_version       INT         NOT NULL DEFAULT 1
);

-- The browse keyset. Matches the read order exactly — sent_at DESC NULLS LAST,
-- then doc_id DESC as the tiebreak — so a cursor page is an index range scan
-- and undated messages sort last instead of dropping out.
CREATE INDEX IF NOT EXISTS email_messages_keyset_idx
    ON email_messages(sent_at DESC NULLS LAST, doc_id DESC);
-- The snapshot watermark: a page taken after a cursor was issued must be able
-- to exclude everything ingested since.
CREATE INDEX IF NOT EXISTS email_messages_ingested_idx
    ON email_messages(ingested_at);
CREATE INDEX IF NOT EXISTS email_messages_message_id_idx
    ON email_messages(message_id) WHERE message_id <> '';

-- Parsed mailboxes, one row per header position. Ordinal preserves header
-- order; a mailbox that could not be parsed keeps its raw text with an empty
-- address rather than being dropped or guessed at.
CREATE TABLE IF NOT EXISTS email_addresses (
    doc_id       UUID    NOT NULL REFERENCES documents(doc_id) ON DELETE CASCADE,
    kind         VARCHAR NOT NULL,
    ordinal      INT     NOT NULL,
    address      TEXT    NOT NULL DEFAULT '',
    display_name TEXT    NOT NULL DEFAULT '',
    raw          TEXT    NOT NULL DEFAULT '',
    valid        BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (doc_id, kind, ordinal)
);

-- Exact sender and recipient filtering. Address first: the question is always
-- "this mailbox, in this role", never "every from address in the corpus". The
-- unparsable rows are excluded because no filter can ever match them.
CREATE INDEX IF NOT EXISTS email_addresses_lookup_idx
    ON email_addresses(address, kind) WHERE address <> '';

CREATE INDEX IF NOT EXISTS email_addresses_sender_domain_idx
    ON email_addresses ((reverse(split_part(reverse(address), '@', 1))))
    WHERE kind = 'from' AND valid AND address <> '';

-- In-Reply-To and References as stored, in header order, exactly as written.
-- These are the exact reply claims a message makes about other messages. They
-- are retained as parsed facts rather than as a grouping: nothing derives a
-- conversation from them any more, and a later source-agnostic topic model
-- would treat them as one high-precision evidence source among several.
CREATE TABLE IF NOT EXISTS email_references (
    doc_id     UUID    NOT NULL REFERENCES documents(doc_id) ON DELETE CASCADE,
    kind       VARCHAR NOT NULL,
    ordinal    INT     NOT NULL,
    message_id TEXT    NOT NULL,
    PRIMARY KEY (doc_id, kind, ordinal)
);

CREATE INDEX IF NOT EXISTS email_references_message_idx
    ON email_references(message_id);

`

// searchIndexName is the lexical leg's BM25 index. Named, not anonymous,
// because to_bm25query's two-argument form (BuildSearchIndex, fuse.go) must
// name the index it scores against.
const searchIndexName = "chunks_bm25_idx"

// BuildSearchIndex creates the lexical leg's BM25 index.
//
// Deliberately not part of schemaSQL. pg_textsearch's own guidance is to
// load data before indexing it — its write path is not yet optimised for
// sustained concurrent inserts, and this application's ingestion is exactly
// that: one row per chunk, streamed continuously by a NATS worker, not a
// single bulk load. Callers build this once, after every write for a run has
// landed (retrieval-design.md §3.3).
//
// 'simple', not 'english': the corpus is bilingual and Postgres cannot select
// a stemmer per row (§4.2). Indexes the chunk's own text only — folding a
// shared subject line in here would make every chunk of a thread match on
// it, the same cross-contamination atomic embedding avoids in the dense leg.
func (d *DB) BuildSearchIndex(ctx context.Context) error {
	_, err := d.Pool.Exec(ctx, fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS %s ON chunks
		 USING bm25 (chunk_text) WITH (text_config='simple')`, searchIndexName))
	if err != nil {
		return fmt.Errorf("build search index: %w", err)
	}
	return nil
}

// SchemaMetadata records what the index was actually built for, plus the one
// workspace_id this database is dedicated to (deviation 34) — recorded here,
// exactly once, rather than on every row of every other table.
type SchemaMetadata struct {
	WorkspaceID  string
	EmbedModel   string
	EmbedDim     int
	TruncatedDim bool
}

// ApplySchema creates the schema with the resolved vector dimension and
// records it. Idempotent.
func (d *DB) ApplySchema(ctx context.Context, meta SchemaMetadata) error {
	if meta.EmbedDim <= 0 {
		return fmt.Errorf("refusing to apply schema with dimension %d", meta.EmbedDim)
	}
	if meta.WorkspaceID == "" {
		return fmt.Errorf("refusing to apply schema with no workspace id")
	}

	existing, err := d.LoadSchemaMetadata(ctx)
	if err == nil {
		if existing.WorkspaceID != meta.WorkspaceID {
			return fmt.Errorf(
				"database is scoped to workspace %q, refusing to apply schema for %q",
				existing.WorkspaceID, meta.WorkspaceID)
		}
		// A dimension change is a re-embed, not a migration: ALTER TABLE
		// cannot reinterpret existing vectors (§4.4).
		if existing.EmbedDim != meta.EmbedDim {
			return fmt.Errorf(
				"schema was built for %s at %d dimensions, endpoint now reports %d; "+
					"this is a re-embed into a new embed_model namespace, not a migration",
				existing.EmbedModel, existing.EmbedDim, meta.EmbedDim)
		}
		// An already-provisioned workspace never re-runs the DDL above, so
		// structural changes reach it here or not at all.
		if err := d.applyPlacementSchema(ctx); err != nil {
			return err
		}
		if err := d.applyEmailSchema(ctx); err != nil {
			return err
		}

		return d.ApplySourceSchema(ctx)
	} else if !errors.Is(err, pgx.ErrNoRows) && !IsUndefinedTable(err) {
		return err
	}

	if _, err := d.Pool.Exec(ctx, fmt.Sprintf(schemaSQL, meta.EmbedDim)); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}

	_, err = d.Pool.Exec(ctx, `
        INSERT INTO schema_metadata (id, workspace_id, embed_model, embed_dim, truncated_dim)
        VALUES (TRUE, $1, $2, $3, $4)
        ON CONFLICT (id) DO NOTHING`,
		meta.WorkspaceID, meta.EmbedModel, meta.EmbedDim, meta.TruncatedDim)
	if err != nil {
		return fmt.Errorf("record schema metadata: %w", err)
	}

	// A no-op here — schemaSQL already carried these statements — and run
	// anyway, so every bootstrap proves the upgrade path is idempotent instead
	// of leaving that only to workspaces old enough to need it.
	if err := d.applyPlacementSchema(ctx); err != nil {
		return err
	}
	if err := d.applyEmailSchema(ctx); err != nil {
		return err
	}
	return d.ApplySourceSchema(ctx)
}

// applyPlacementSchema upgrades a workspace with containment tracking.
//
// Structural, plus one seed: every parent_doc_id that exists is a containment
// that certainly happened, so it becomes a placement. That seed is a floor and
// not the truth — it reproduces the single-parent under-count the table exists
// to fix — which is why placements_backfilled_at stays NULL until the
// containment backfill has re-derived the rest from verified originals, and why pruning
// refuses to run while it is.
//
// It also converts documents.parent_doc_id from ON DELETE CASCADE to SET NULL.
// Cascade would overrule the whole point of the table: deleting a container
// would take a shared attachment with it before the sweep ever judged whether
// anything else still held it.
func (d *DB) applyPlacementSchema(ctx context.Context) error {
	if _, err := d.Pool.Exec(ctx, placementSchemaSQL); err != nil {
		return fmt.Errorf("apply placement schema: %w", err)
	}
	return nil
}

// ApplyPlacementSchema upgrades containment and deletion state without an
// embedding probe. Maintenance commands use the existing workspace schema.
func (d *DB) ApplyPlacementSchema(ctx context.Context) error {
	return d.applyPlacementSchema(ctx)
}

// applyEmailSchema creates the email browse and conversation tables on a
// workspace that was provisioned before they existed.
//
// Purely structural: it adds tables and indexes and rewrites nothing, so there
// is no partial state to guard against and no backfill to sequence. Existing
// rows gain email metadata when their messages are re-ingested from verified originals,
// which is the supported reprocessing path — this migration deliberately does
// not synthesise metadata for documents whose bytes it has not read.
func (d *DB) applyEmailSchema(ctx context.Context) error {
	if _, err := d.Pool.Exec(ctx, emailSchemaSQL); err != nil {
		return fmt.Errorf("apply email schema: %w", err)
	}
	return nil
}

// LoadSchemaMetadata reads what the index was built for. Every worker calls
// this at startup and treats a mismatch as fatal (§4.4).
func (d *DB) LoadSchemaMetadata(ctx context.Context) (SchemaMetadata, error) {
	var m SchemaMetadata
	err := d.Pool.QueryRow(ctx,
		`SELECT COALESCE(workspace_id, ''), embed_model, embed_dim, truncated_dim
         FROM schema_metadata WHERE id`).
		Scan(&m.WorkspaceID, &m.EmbedModel, &m.EmbedDim, &m.TruncatedDim)
	return m, err
}

// VerifyDimension is the fatal startup check. A worker that embeds at one
// dimension into a column sized for another writes vectors that are silently
// not comparable to their neighbours.
func (d *DB) VerifyDimension(ctx context.Context, model string, dim int) error {
	m, err := d.LoadSchemaMetadata(ctx)
	if err != nil {
		return fmt.Errorf("read schema metadata (has schema-bootstrap run?): %w", err)
	}
	if m.EmbedDim != dim {
		return fmt.Errorf("FATAL: index built at %d dimensions, endpoint reports %d", m.EmbedDim, dim)
	}
	if m.EmbedModel != model {
		return fmt.Errorf("FATAL: index built for model %q, endpoint serves %q", m.EmbedModel, model)
	}
	return nil
}

// IsUndefinedTable reports whether err is Postgres's "relation does not
// exist" (42P01) — the shape LoadSchemaMetadata's caller sees against a
// database that has never had ApplySchema run against it at all.
func IsUndefinedTable(err error) bool {
	return err != nil && (contains(err.Error(), "42P01") || contains(err.Error(), "does not exist"))
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
