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
const schemaSQL = `
CREATE EXTENSION IF NOT EXISTS vector;

DO $$ BEGIN
    CREATE TYPE processing_status AS ENUM
        ('PENDING','PROCESSING','COMPLETED','SKIPPED','FAILED');
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

CREATE TABLE IF NOT EXISTS schema_metadata (
    id            BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    embed_model   VARCHAR NOT NULL,
    embed_dim     INT     NOT NULL,
    truncated_dim BOOLEAN NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS documents (
    doc_id            UUID PRIMARY KEY,
    parent_doc_id     UUID REFERENCES documents(doc_id) ON DELETE CASCADE,
    workspace_id      VARCHAR NOT NULL,
    collection_id     VARCHAR NOT NULL DEFAULT '',
    thread_id         VARCHAR NOT NULL DEFAULT '',
    processing_status processing_status NOT NULL DEFAULT 'PENDING',
    doc_type          VARCHAR NOT NULL DEFAULT '',
    mime_type         VARCHAR NOT NULL DEFAULT '',
    rustfs_raw_uri    TEXT    NOT NULL DEFAULT '',
    raw_sha256        VARCHAR NOT NULL DEFAULT '',
    source_filename   TEXT    NOT NULL DEFAULT '',
    normalized_text   TEXT,
    metadata_headers  JSONB   NOT NULL DEFAULT '{}'::jsonb,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS documents_workspace_idx  ON documents(workspace_id);
CREATE INDEX IF NOT EXISTS documents_collection_idx ON documents(collection_id);
CREATE INDEX IF NOT EXISTS documents_thread_idx     ON documents(thread_id);
CREATE INDEX IF NOT EXISTS documents_parent_idx     ON documents(parent_doc_id);
-- Drives the stale-PENDING reconciliation sweep (§2.2).
CREATE INDEX IF NOT EXISTS documents_pending_idx
    ON documents(updated_at) WHERE processing_status = 'PENDING';
-- Drives the bucket-scan anti-join (§5.2).
CREATE INDEX IF NOT EXISTS documents_raw_uri_idx    ON documents(rustfs_raw_uri);

CREATE TABLE IF NOT EXISTS document_chunks (
    chunk_id          UUID PRIMARY KEY,
    doc_id            UUID NOT NULL REFERENCES documents(doc_id) ON DELETE CASCADE,
    workspace_id      VARCHAR NOT NULL,
    chunk_index       INT     NOT NULL,
    start_char_offset INT     NOT NULL,
    end_char_offset   INT     NOT NULL,
    chunk_text        TEXT    NOT NULL,
    embed_model       VARCHAR NOT NULL,
    embedding         halfvec(%[1]d),
    -- 'simple', not 'english': the corpus is bilingual and Postgres cannot
    -- select a stemmer per row (§4.2).
    fulltext_search   TSVECTOR GENERATED ALWAYS AS
                          (to_tsvector('simple', chunk_text)) STORED
);

CREATE INDEX IF NOT EXISTS chunks_doc_idx       ON document_chunks(doc_id);
CREATE INDEX IF NOT EXISTS chunks_workspace_idx ON document_chunks(workspace_id, embed_model);
CREATE INDEX IF NOT EXISTS chunks_fts_idx       ON document_chunks USING GIN (fulltext_search);
CREATE INDEX IF NOT EXISTS chunks_hnsw_idx      ON document_chunks
    USING hnsw (embedding halfvec_cosine_ops) WITH (m = 16, ef_construction = 64);
`

// SchemaMetadata records what the index was actually built for.
type SchemaMetadata struct {
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

	existing, err := d.LoadSchemaMetadata(ctx)
	if err == nil {
		// A dimension change is a re-embed, not a migration: ALTER TABLE
		// cannot reinterpret existing vectors (§4.4).
		if existing.EmbedDim != meta.EmbedDim {
			return fmt.Errorf(
				"schema was built for %s at %d dimensions, endpoint now reports %d; "+
					"this is a re-embed into a new embed_model namespace, not a migration",
				existing.EmbedModel, existing.EmbedDim, meta.EmbedDim)
		}
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) && !isUndefinedTable(err) {
		return err
	}

	if _, err := d.Pool.Exec(ctx, fmt.Sprintf(schemaSQL, meta.EmbedDim)); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}

	_, err = d.Pool.Exec(ctx, `
        INSERT INTO schema_metadata (id, embed_model, embed_dim, truncated_dim)
        VALUES (TRUE, $1, $2, $3)
        ON CONFLICT (id) DO NOTHING`,
		meta.EmbedModel, meta.EmbedDim, meta.TruncatedDim)
	if err != nil {
		return fmt.Errorf("record schema metadata: %w", err)
	}
	return nil
}

// LoadSchemaMetadata reads what the index was built for. Every worker calls
// this at startup and treats a mismatch as fatal (§4.4).
func (d *DB) LoadSchemaMetadata(ctx context.Context) (SchemaMetadata, error) {
	var m SchemaMetadata
	err := d.Pool.QueryRow(ctx,
		`SELECT embed_model, embed_dim, truncated_dim FROM schema_metadata WHERE id`).
		Scan(&m.EmbedModel, &m.EmbedDim, &m.TruncatedDim)
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

func isUndefinedTable(err error) bool {
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
