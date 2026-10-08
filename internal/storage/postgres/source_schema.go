package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/suankan/pocket-advisor/internal/domain"
)

// queueSchemaVersion is recorded in application_migrations for the operator's
// benefit and decides nothing. Whether the queue layout is present is read from
// ingestion_jobs itself (queueLayoutCurrent): other releases have written their
// own rows into that table, so a version number cannot say what this step
// needs to know — a workspace carrying a row with this number from an earlier
// release must still be upgraded.
const queueSchemaVersion = 3

const sourceSchemaSQL = `
CREATE TABLE IF NOT EXISTS application_migrations (version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now());
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
CREATE INDEX IF NOT EXISTS source_occurrences_scan_idx ON source_occurrences(scan_id);
CREATE TABLE IF NOT EXISTS source_members (
 parent_id uuid NOT NULL REFERENCES documents ON DELETE CASCADE,
 selector text NOT NULL, child_id uuid NOT NULL REFERENCES documents ON DELETE CASCADE,
 PRIMARY KEY(parent_id,selector));
CREATE INDEX IF NOT EXISTS source_members_child_idx ON source_members(child_id);
ALTER TABLE documents ADD COLUMN IF NOT EXISTS extraction_hash text NOT NULL DEFAULT '';
ALTER TABLE documents ADD COLUMN IF NOT EXISTS extraction_version text NOT NULL DEFAULT '';
ALTER TABLE document_chunks ADD COLUMN IF NOT EXISTS slice_sha256 bytea;
CREATE TABLE IF NOT EXISTS ingestion_jobs (
 job_id uuid PRIMARY KEY DEFAULT gen_random_uuid(), doc_id uuid NOT NULL REFERENCES documents ON DELETE CASCADE,
 subject text NOT NULL, revision text NOT NULL, payload bytea NOT NULL, traceparent text NOT NULL,
 state text NOT NULL DEFAULT 'READY',
 generation bigint NOT NULL DEFAULT 1, attempts integer NOT NULL DEFAULT 0,
 available_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 lease_until timestamptz, claimed_at timestamptz, claimed_by text,
 created_at timestamptz NOT NULL DEFAULT now(), finished_at timestamptz,
 reason text NOT NULL DEFAULT '', last_failure_reason text NOT NULL DEFAULT '',
 UNIQUE(doc_id,subject,revision));
INSERT INTO application_migrations(version) VALUES(1) ON CONFLICT DO NOTHING;
`

// ApplySourceSchema applies the source inventory, containment and durable-job
// half of the workspace schema. It is transactional and repeatable, and it
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
		return d.applyQueueSchema(ctx)
	})
}

// queueSchemaSQL converts ingestion_jobs from the broker-dispatch layout to the
// queue layout. It also runs against a database created from the current
// table definition, where the column statements are no-ops, so fresh and
// upgraded databases converge on one path. It runs only while the table does
// not already have the queue layout, so a reapplication cannot reset attempts,
// generations or leases.
//
// Indexes carry new names: the old ingestion_jobs_ready_idx is dropped rather
// than kept through an IF NOT EXISTS that would silently preserve its
// created_at-only definition.
const queueSchemaSQL = `
ALTER TABLE ingestion_jobs ADD COLUMN IF NOT EXISTS attempts integer NOT NULL DEFAULT 0;
ALTER TABLE ingestion_jobs ADD COLUMN IF NOT EXISTS lease_until timestamptz;
ALTER TABLE ingestion_jobs ADD COLUMN IF NOT EXISTS available_at timestamptz;
ALTER TABLE ingestion_jobs ADD COLUMN IF NOT EXISTS claimed_at timestamptz;
ALTER TABLE ingestion_jobs ADD COLUMN IF NOT EXISTS claimed_by text;
ALTER TABLE ingestion_jobs ADD COLUMN IF NOT EXISTS last_failure_reason text NOT NULL DEFAULT '';
UPDATE ingestion_jobs SET available_at = created_at WHERE available_at IS NULL;
ALTER TABLE ingestion_jobs ALTER COLUMN available_at SET DEFAULT clock_timestamp();
ALTER TABLE ingestion_jobs ALTER COLUMN available_at SET NOT NULL;
UPDATE ingestion_jobs SET last_failure_reason = reason
 WHERE state = 'FAILED' AND last_failure_reason = '' AND reason IN (%s);
ALTER TABLE ingestion_jobs DROP CONSTRAINT IF EXISTS ingestion_jobs_state_check;
UPDATE ingestion_jobs SET state = 'READY', generation = generation + 1, lease_until = NULL WHERE state = 'DISPATCHED';
ALTER TABLE ingestion_jobs ADD CONSTRAINT ingestion_jobs_state_check
 CHECK (state IN ('READY','RUNNING','COMPLETED','FAILED','WAITING_SOURCE'));
ALTER TABLE ingestion_jobs DROP CONSTRAINT IF EXISTS ingestion_jobs_attempts_check;
ALTER TABLE ingestion_jobs ADD CONSTRAINT ingestion_jobs_attempts_check CHECK (attempts BETWEEN 0 AND 3);
ALTER TABLE ingestion_jobs DROP CONSTRAINT IF EXISTS ingestion_jobs_lease_check;
ALTER TABLE ingestion_jobs ADD CONSTRAINT ingestion_jobs_lease_check
 CHECK ((state = 'RUNNING') = (lease_until IS NOT NULL));
ALTER TABLE ingestion_jobs DROP COLUMN IF EXISTS dispatched_at;
ALTER TABLE ingestion_jobs DROP COLUMN IF EXISTS dispatch_lease_until;
DROP INDEX IF EXISTS ingestion_jobs_ready_idx;
CREATE INDEX IF NOT EXISTS ingestion_jobs_claim_ready_idx
 ON ingestion_jobs(subject, available_at, created_at, job_id) WHERE state = 'READY';
CREATE INDEX IF NOT EXISTS ingestion_jobs_claim_expired_idx
 ON ingestion_jobs(subject, lease_until, created_at, job_id) WHERE state = 'RUNNING';
INSERT INTO application_migrations(version) VALUES(%d) ON CONFLICT DO NOTHING;
`

// queueLayoutCurrentSQL is true only for a complete queue layout: the lease
// column present, the dispatch columns gone, the lease constraint installed and
// both claim indexes created. A fresh table has the columns but not the
// constraint or indexes, so it still runs the step.
const queueLayoutCurrentSQL = `SELECT
 to_regclass('ingestion_jobs') IS NOT NULL
 AND EXISTS (SELECT 1 FROM information_schema.columns
             WHERE table_schema = current_schema() AND table_name = 'ingestion_jobs' AND column_name = 'lease_until')
 AND NOT EXISTS (SELECT 1 FROM information_schema.columns
             WHERE table_schema = current_schema() AND table_name = 'ingestion_jobs' AND column_name = 'dispatched_at')
 AND EXISTS (SELECT 1 FROM pg_constraint
             WHERE conname = 'ingestion_jobs_lease_check' AND conrelid = to_regclass('ingestion_jobs'))
 AND to_regclass('ingestion_jobs_claim_ready_idx') IS NOT NULL
 AND to_regclass('ingestion_jobs_claim_expired_idx') IS NOT NULL`

// applyQueueSchema runs the queue layout step unless the table already has it.
// The caller holds the schema advisory lock inside its transaction.
func (d *DB) applyQueueSchema(ctx context.Context) error {
	var done bool
	if err := d.executor(ctx).QueryRow(ctx, queueLayoutCurrentSQL).Scan(&done); err != nil {
		return fmt.Errorf("queue layout: %w", err)
	}
	if done {
		return nil
	}
	reasons := make([]string, 0, 10)
	for _, r := range domain.ProcessingFailureReasons() {
		reasons = append(reasons, "'"+string(r)+"'")
	}
	if _, err := d.executor(ctx).Exec(ctx, fmt.Sprintf(queueSchemaSQL, strings.Join(reasons, ","), queueSchemaVersion)); err != nil {
		return fmt.Errorf("queue schema: %w", err)
	}
	return nil
}

// QueueSchemaReady reports whether the database carries the queue layout. It
// is read-only so doctor and recovery can diagnose an old layout without
// running a job query the old columns cannot answer.
func (d *DB) QueueSchemaReady(ctx context.Context) (bool, error) {
	var ready bool
	err := d.Pool.QueryRow(ctx, queueLayoutCurrentSQL).Scan(&ready)
	return ready, err
}
