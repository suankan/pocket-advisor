package postgres

import (
	"context"
	"fmt"
)

const sourceSchemaSQL = `
CREATE TABLE IF NOT EXISTS application_migrations (version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now());
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='documents' AND column_name='rustfs_raw_uri') THEN
  ALTER TABLE documents RENAME COLUMN rustfs_raw_uri TO source_ref;
 END IF;
END $$;
DO $$ BEGIN
 IF to_regclass('pending_object_deletions') IS NOT NULL THEN
  IF EXISTS(SELECT 1 FROM pending_object_deletions) THEN
   RAISE EXCEPTION 'finish pending object deletions with the previous binary before upgrading';
  END IF;
  DROP TABLE pending_object_deletions;
 END IF;
END $$;
CREATE TABLE IF NOT EXISTS source_roots (
 root_id uuid PRIMARY KEY DEFAULT gen_random_uuid(), binding_key text NOT NULL UNIQUE);
CREATE TABLE IF NOT EXISTS source_scans (
 scan_id uuid PRIMARY KEY DEFAULT gen_random_uuid(), root_keys text[] NOT NULL,
 started_at timestamptz NOT NULL DEFAULT now(), completed_at timestamptz);
CREATE TABLE IF NOT EXISTS source_occurrences (
 root_id uuid NOT NULL REFERENCES source_roots ON DELETE CASCADE,
 relative_path text NOT NULL, display_path text NOT NULL,
 doc_id uuid NOT NULL REFERENCES documents ON DELETE CASCADE,
 byte_size bigint NOT NULL, modified_at timestamptz NOT NULL,
 scan_id uuid NOT NULL REFERENCES source_scans, observed_at timestamptz NOT NULL DEFAULT now(),
 available boolean NOT NULL DEFAULT true, PRIMARY KEY(root_id,relative_path));
CREATE INDEX IF NOT EXISTS source_occurrences_document_idx ON source_occurrences(doc_id) WHERE available;
CREATE TABLE IF NOT EXISTS source_members (
 parent_id uuid NOT NULL REFERENCES documents ON DELETE CASCADE,
 selector text NOT NULL, child_id uuid NOT NULL REFERENCES documents ON DELETE CASCADE,
 PRIMARY KEY(parent_id,selector));
CREATE INDEX IF NOT EXISTS source_members_child_idx ON source_members(child_id);
-- The object-bucket anti-join it served is gone; source_ref is never a predicate.
DROP INDEX IF EXISTS documents_source_ref_idx;
ALTER TABLE documents ADD COLUMN IF NOT EXISTS extraction_hash text NOT NULL DEFAULT '';
ALTER TABLE documents ADD COLUMN IF NOT EXISTS extraction_version text NOT NULL DEFAULT '';
ALTER TABLE document_chunks ADD COLUMN IF NOT EXISTS slice_sha256 bytea;
CREATE TABLE IF NOT EXISTS ingestion_jobs (
 job_id uuid PRIMARY KEY DEFAULT gen_random_uuid(), doc_id uuid NOT NULL REFERENCES documents ON DELETE CASCADE,
 subject text NOT NULL, revision text NOT NULL, payload bytea NOT NULL, traceparent text NOT NULL,
 state text NOT NULL DEFAULT 'READY' CHECK(state IN ('READY','DISPATCHED','COMPLETED','FAILED','WAITING_SOURCE')),
 generation bigint NOT NULL DEFAULT 1, dispatched_at timestamptz, dispatch_lease_until timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(), finished_at timestamptz,
 reason text NOT NULL DEFAULT '', UNIQUE(doc_id,subject,revision));
CREATE INDEX IF NOT EXISTS ingestion_jobs_ready_idx ON ingestion_jobs(created_at) WHERE state='READY';
UPDATE documents SET extraction_hash=encode(sha256(convert_to(normalized_text,'UTF8')),'hex'),extraction_version='legacy-canonical' WHERE normalized_text IS NOT NULL AND extraction_hash='';
UPDATE documents SET source_ref='source:'||doc_id::text WHERE source_ref NOT LIKE 'source:%';
INSERT INTO application_migrations(version) VALUES(1) ON CONFLICT DO NOTHING;
`

// ApplySourceSchema is a transactional, repeatable structural migration. It
// preserves document identities; source coverage is verified separately.
func (d *DB) ApplySourceSchema(ctx context.Context) error {
	return d.Transaction(ctx, func(ctx context.Context) error {
		if _, err := d.executor(ctx).Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('source-schema'))"); err != nil {
			return err
		}
		_, err := d.executor(ctx).Exec(ctx, sourceSchemaSQL)
		if err != nil {
			return fmt.Errorf("source schema: %w", err)
		}
		return nil
	})
}
