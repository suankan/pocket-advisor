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

CREATE INDEX IF NOT EXISTS documents_workspace_idx  ON documents(workspace_id);
CREATE INDEX IF NOT EXISTS documents_collection_idx ON documents(collection_id);
CREATE INDEX IF NOT EXISTS documents_thread_idx     ON documents(thread_id);
CREATE INDEX IF NOT EXISTS documents_parent_idx     ON documents(parent_doc_id);
-- Drives the stale-PENDING reconciliation sweep (§2.2).
CREATE INDEX IF NOT EXISTS documents_pending_idx
    ON documents(updated_at) WHERE processing_status = 'PENDING';
-- Drives the bucket-scan anti-join (§5.2).
CREATE INDEX IF NOT EXISTS documents_raw_uri_idx    ON documents(rustfs_raw_uri);
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
-- does not.
--
-- Identity is scoped by embed_model as well as workspace: a re-embed writes a
-- new model namespace, and collapsing across namespaces would serve a vector
-- from the wrong model. Equality only — never a similarity threshold. Measured
-- on this corpus, chunks naming different monthly statement periods sit at
-- cosine 0.991 to 0.993, so any threshold loose enough to merge reformatted
-- text also merges statements that must stay distinct
-- (docs/tasks/p0-0-dilution-by-duplicated-chunks.md).
CREATE TABLE IF NOT EXISTS chunks (
    content_id   UUID PRIMARY KEY,
    workspace_id VARCHAR NOT NULL,
    embed_model  VARCHAR NOT NULL,
    content_hash BYTEA   NOT NULL,
    chunk_text   TEXT    NOT NULL,
    embedding    halfvec(%[1]d),
    UNIQUE (workspace_id, embed_model, content_hash)
);

CREATE INDEX IF NOT EXISTS chunks_hnsw_idx ON chunks
    USING hnsw (embedding halfvec_cosine_ops) WITH (m = 16, ef_construction = 64);

-- Where a passage sits in one document. Offsets and ordinal are per-document
-- and stay here: the same passage occupies different byte ranges in each
-- document that contains it, so folding them onto the shared row would make a
-- citation resolve against the wrong text.
--
-- chunk_id remains the identity of a passage-in-a-document, which is what
-- retrieval ranks and what a packet cites. One row per placement, exactly as
-- before deduplication, so the read path sees the same candidates it always
-- did — only the text and vector behind them are now shared.
CREATE TABLE IF NOT EXISTS document_chunks (
    chunk_id          UUID PRIMARY KEY,
    doc_id            UUID NOT NULL REFERENCES documents(doc_id) ON DELETE CASCADE,
    content_id        UUID NOT NULL REFERENCES chunks(content_id) ON DELETE RESTRICT,
    workspace_id      VARCHAR NOT NULL,
    chunk_index       INT     NOT NULL,
    start_char_offset INT     NOT NULL,
    end_char_offset   INT     NOT NULL
);

CREATE INDEX IF NOT EXISTS chunks_doc_idx        ON document_chunks(doc_id);
CREATE INDEX IF NOT EXISTS chunks_workspace_idx  ON document_chunks(workspace_id);
CREATE INDEX IF NOT EXISTS chunks_content_idx    ON document_chunks(content_id);
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

// DropSearchIndex removes the BM25 index before a bulk ingest run begins, so
// pg_textsearch never has to maintain it incrementally against a stream of
// individual inserts — see BuildSearchIndex.
func (d *DB) DropSearchIndex(ctx context.Context) error {
	_, err := d.Pool.Exec(ctx, fmt.Sprintf(`DROP INDEX IF EXISTS %s`, searchIndexName))
	if err != nil {
		return fmt.Errorf("drop search index: %w", err)
	}
	return nil
}

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
		// An already-provisioned workspace never re-runs the DDL above, so
		// structural changes reach it here or not at all.
		return d.migrateChunkContent(ctx, existing.EmbedDim)
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

// migrateChunkContent moves passage text and vectors off document_chunks onto
// the shared chunks table, collapsing duplicates.
//
// Idempotent and a no-op once done: it keys off document_chunks still having a
// chunk_text column. Existing embeddings are carried across unchanged, so this
// is a migration rather than a re-embed — the text is not re-chunked and the
// vectors are not recomputed. One passage's surviving row is chosen by lowest
// chunk_id purely so the choice is deterministic; every copy holds the same
// text and the same vector, which is what makes them duplicates.
//
// Runs in one transaction. A partial migration would leave the read path
// querying columns that no longer exist, so this either lands whole or not at
// all.
func (d *DB) migrateChunkContent(ctx context.Context, dim int) error {
	var legacy bool
	if err := d.Pool.QueryRow(ctx, `
        SELECT EXISTS (
            SELECT 1 FROM information_schema.columns
            WHERE table_name = 'document_chunks' AND column_name = 'chunk_text')`,
	).Scan(&legacy); err != nil {
		return fmt.Errorf("detect chunk schema: %w", err)
	}
	if !legacy {
		return nil
	}

	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin chunk migration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// hashOf must match contentHash in chunk_repo.go exactly: the write path
	// and this backfill have to agree on what counts as the same passage, or a
	// migrated workspace would keep inserting duplicates of what it holds.
	// Parameterised by column because both tables carry chunk_text while the
	// migration is in flight, which makes a bare reference ambiguous.
	hashOf := func(col string) string {
		return fmt.Sprintf(
			`sha256(convert_to(btrim(regexp_replace(%s, '\s+', ' ', 'g')), 'UTF8'))`, col)
	}
	normalisedHashSQL := hashOf("chunk_text")

	steps := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS chunks (
            content_id   UUID PRIMARY KEY,
            workspace_id VARCHAR NOT NULL,
            embed_model  VARCHAR NOT NULL,
            content_hash BYTEA   NOT NULL,
            chunk_text   TEXT    NOT NULL,
            embedding    halfvec(%d),
            UNIQUE (workspace_id, embed_model, content_hash))`, dim),

		fmt.Sprintf(`INSERT INTO chunks
            (content_id, workspace_id, embed_model, content_hash, chunk_text, embedding)
         SELECT DISTINCT ON (workspace_id, embed_model, %[1]s)
                gen_random_uuid(), workspace_id, embed_model, %[1]s, chunk_text, embedding
         FROM document_chunks
         ORDER BY workspace_id, embed_model, %[1]s, chunk_id`, normalisedHashSQL),

		`ALTER TABLE document_chunks ADD COLUMN IF NOT EXISTS content_id UUID`,

		fmt.Sprintf(`UPDATE document_chunks dc SET content_id = c.content_id
         FROM chunks c
         WHERE c.workspace_id = dc.workspace_id
           AND c.embed_model  = dc.embed_model
           AND c.content_hash = %s`, hashOf("dc.chunk_text")),

		`ALTER TABLE document_chunks ALTER COLUMN content_id SET NOT NULL`,
		`ALTER TABLE document_chunks ADD CONSTRAINT document_chunks_content_fk
             FOREIGN KEY (content_id) REFERENCES chunks(content_id) ON DELETE RESTRICT`,

		// The old indexes name columns that are about to disappear.
		`DROP INDEX IF EXISTS chunks_hnsw_idx`,
		`DROP INDEX IF EXISTS ` + searchIndexName,
		`DROP INDEX IF EXISTS chunks_workspace_idx`,

		`ALTER TABLE document_chunks
             DROP COLUMN chunk_text,
             DROP COLUMN embed_model,
             DROP COLUMN embedding`,

		`CREATE INDEX chunks_hnsw_idx ON chunks
             USING hnsw (embedding halfvec_cosine_ops) WITH (m = 16, ef_construction = 64)`,
		`CREATE INDEX chunks_workspace_idx ON document_chunks(workspace_id)`,
		`CREATE INDEX IF NOT EXISTS chunks_content_idx ON document_chunks(content_id)`,

		// Rebuilt here rather than left to the next ingest. BuildSearchIndex is
		// normally deferred until after a bulk load, but this migration dropped
		// a populated index and the rows are already in place: leaving it out
		// would strand the workspace with no lexical leg until something
		// happened to re-ingest it.
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON chunks
             USING bm25 (chunk_text) WITH (text_config='simple')`, searchIndexName),
	}

	for i, stmt := range steps {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("chunk migration step %d: %w", i+1, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit chunk migration: %w", err)
	}

	// Outside the transaction because VACUUM cannot run inside one, and not
	// optional: DROP COLUMN only marks a column dropped, and the content_id
	// backfill leaves a dead tuple per row, so without this the table keeps
	// every byte the migration was meant to remove. Measured on a real
	// workspace the placement heap stood at 42 MB immediately after committing
	// and 2 MB after this ran.
	//
	// A failure here costs space, not correctness, so it is reported rather
	// than returned: the migration itself has already landed.
	if _, err := d.Pool.Exec(ctx, `VACUUM FULL document_chunks`); err != nil {
		return fmt.Errorf("chunk migration committed, but reclaiming space failed "+
			"(run VACUUM FULL document_chunks by hand): %w", err)
	}
	return nil
}
