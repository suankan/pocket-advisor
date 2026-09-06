package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/suankan/pocket-advisor/internal/domain"
)

// RecordPlacement notes that one document contains another.
//
// Called for every containment an extraction observes, including the ones
// whose content was already known: that second, third and fourth observation
// is exactly the information documents.parent_doc_id throws away, and the
// only thing that can later say a shared attachment is still being held.
// Repeat calls are expected — the same email is re-extracted on redelivery —
// so a placement that already exists is success, not a conflict.
func (r *DocumentRepo) RecordPlacement(ctx context.Context, containerID, containedID string) error {
	if containerID == "" || containedID == "" {
		return fmt.Errorf("record placement: empty document id")
	}
	// A document containing itself would make the retention closure trivially
	// true for it and is never meaningful. Content identity is unique per
	// workspace, so this can only arise from a caller mistake.
	if containerID == containedID {
		return fmt.Errorf("record placement: document %s cannot contain itself", containerID)
	}
	_, err := r.db.Pool.Exec(ctx, `
        INSERT INTO document_placements (container_doc_id, contained_doc_id)
        VALUES ($1, $2)
        ON CONFLICT DO NOTHING`, containerID, containedID)
	if err != nil {
		return fmt.Errorf("record placement %s -> %s: %w", containerID, containedID, err)
	}
	return nil
}

// DocIDBySHA resolves content to the document that owns it, or "" when the
// content is not known to this workspace.
func (r *DocumentRepo) DocIDBySHA(ctx context.Context, sha string) (string, error) {
	var id string
	err := r.db.Pool.QueryRow(ctx,
		`SELECT doc_id::text FROM documents WHERE raw_sha256 = $1`, sha).Scan(&id)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("lookup document by content %s: %w", sha, err)
	}
	return id, nil
}

// PlacementsBackfilledAt reports when the containment backfill last completed.
// A zero time means it never has, and pruning must refuse.
func (r *DocumentRepo) PlacementsBackfilledAt(ctx context.Context) (time.Time, error) {
	var at *time.Time
	err := r.db.Pool.QueryRow(ctx,
		`SELECT placements_backfilled_at FROM schema_metadata LIMIT 1`).Scan(&at)
	if err == pgx.ErrNoRows {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("read backfill marker: %w", err)
	}
	if at == nil {
		return time.Time{}, nil
	}
	return *at, nil
}

// MarkPlacementsBackfilled records that every existing document's containments
// have been re-derived from Tier 1.
func (r *DocumentRepo) MarkPlacementsBackfilled(ctx context.Context, at time.Time) error {
	_, err := r.db.Pool.Exec(ctx,
		`UPDATE schema_metadata SET placements_backfilled_at = $1`, at)
	if err != nil {
		return fmt.Errorf("mark backfill complete: %w", err)
	}
	return nil
}

// PruneCandidate is one document the sweep found nothing holding.
type PruneCandidate struct {
	DocID   string
	SHA256  string
	DocType string
	// SourcePath is set only for a document staged in its own right; a
	// contained document never had a file of its own. SourceName is what makes
	// the latter recognisable to an operator reading a deletion plan.
	SourcePath string
	SourceName string
}

// UnreachableDocuments returns every document not reachable from staged
// content, given the set of content hashes currently present on disk.
//
// This is reachability, not reference counting. Containment nests — an archive
// inside an email, an email attached to an email — so removing a container can
// orphan a whole subtree, and a single pass over direct references would leave
// the tail behind. Asking instead which documents are still reachable from
// something staged answers the whole graph at once, and a UNION recursive term
// terminates on its own even if malformed lineage ever produced a cycle.
//
// The staged set arrives as a temporary table rather than a parameter list
// because a real workspace stages tens of thousands of files, and a query with
// 60,000 bind parameters is not a query.
func (r *DocumentRepo) UnreachableDocuments(ctx context.Context, stagedSHAs []string) ([]PruneCandidate, error) {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin sweep: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
        CREATE TEMP TABLE staged_sha (sha VARCHAR PRIMARY KEY) ON COMMIT DROP`); err != nil {
		return nil, fmt.Errorf("stage content set: %w", err)
	}
	rows := make([][]any, 0, len(stagedSHAs))
	seen := make(map[string]struct{}, len(stagedSHAs))
	for _, s := range stagedSHAs {
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		rows = append(rows, []any{s})
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"staged_sha"}, []string{"sha"},
		pgx.CopyFromRows(rows)); err != nil {
		return nil, fmt.Errorf("load staged content set: %w", err)
	}

	result, err := tx.Query(ctx, `
        WITH RECURSIVE retained AS (
            SELECT d.doc_id
            FROM documents d
            JOIN staged_sha s ON s.sha = d.raw_sha256
            UNION
            SELECT p.contained_doc_id
            FROM document_placements p
            JOIN retained r ON p.container_doc_id = r.doc_id
        )
        SELECT d.doc_id::text, d.raw_sha256, d.doc_type,
               COALESCE(d.metadata_headers->>'source_path', ''), d.source_filename
        FROM documents d
        WHERE NOT EXISTS (SELECT 1 FROM retained r WHERE r.doc_id = d.doc_id)
        ORDER BY COALESCE(d.metadata_headers->>'source_path', ''), d.source_filename`)
	if err != nil {
		return nil, fmt.Errorf("sweep: %w", err)
	}
	defer result.Close()

	var out []PruneCandidate
	for result.Next() {
		var c PruneCandidate
		if err := result.Scan(&c.DocID, &c.SHA256, &c.DocType, &c.SourcePath, &c.SourceName); err != nil {
			return nil, fmt.Errorf("read sweep candidate: %w", err)
		}
		out = append(out, c)
	}
	if err := result.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// CountDocuments reports how many documents the workspace holds, which is the
// denominator the sweep's blast-radius guard compares against.
func (r *DocumentRepo) CountDocuments(ctx context.Context) (int, error) {
	var n int
	if err := r.db.Pool.QueryRow(ctx, `SELECT count(*) FROM documents`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count documents: %w", err)
	}
	return n, nil
}

// DeleteDocuments removes documents by id and repoints any survivor whose
// provenance pointed at one of them.
//
// The repointing matters: parent_doc_id is how the read path finds the nearest
// ancestor holding a source path, and the FK sets it to NULL when its target
// goes. A document that survived because something else still contains it
// would otherwise keep the deletion's mark forever, unable to say where it
// came from. Any surviving container is a truthful answer, so the query takes
// one.
func (r *DocumentRepo) DeleteDocuments(ctx context.Context, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin delete: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return deleteDocuments(ctx, tx, ids)
}

// deleteDocuments commits row removal and its object cleanup requests together.
func deleteDocuments(ctx context.Context, tx pgx.Tx, ids []string) (int64, error) {
	if _, err := tx.Exec(ctx, `
        INSERT INTO pending_object_deletions (raw_sha256)
        SELECT raw_sha256 FROM documents WHERE doc_id = ANY($1::uuid[])
        ON CONFLICT DO NOTHING`, ids); err != nil {
		return 0, fmt.Errorf("queue object deletion: %w", err)
	}
	if _, err := tx.Exec(ctx, `
        UPDATE documents d
        SET parent_doc_id = (
              SELECT q.container_doc_id FROM document_placements q
              WHERE q.contained_doc_id = d.doc_id
                AND NOT (q.container_doc_id = ANY($1::uuid[]))
              ORDER BY q.first_seen_at, q.container_doc_id
              LIMIT 1
            ), updated_at = now()
        WHERE d.parent_doc_id = ANY($1::uuid[])
          AND NOT (d.doc_id = ANY($1::uuid[]))`, ids); err != nil {
		return 0, fmt.Errorf("repoint survivors: %w", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM documents WHERE doc_id = ANY($1::uuid[])`, ids)
	if err != nil {
		return 0, fmt.Errorf("delete documents: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit delete: %w", err)
	}
	return tag.RowsAffected(), nil
}

// PendingObjectDeletions returns a bounded batch of durable cleanup work.
func (r *DocumentRepo) PendingObjectDeletions(ctx context.Context, limit int) ([]string, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT raw_sha256 FROM pending_object_deletions
		ORDER BY raw_sha256 LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (r *DocumentRepo) CompleteObjectDeletion(ctx context.Context, sha string) error {
	_, err := r.db.Pool.Exec(ctx, `DELETE FROM pending_object_deletions WHERE raw_sha256 = $1`, sha)
	return err
}

// ClearObjectDeletions follows a successful whole-bucket wipe. The conditional
// keeps --delete-data usable on workspaces predating the cleanup table.
func (r *DocumentRepo) ClearObjectDeletions(ctx context.Context) error {
	_, err := r.db.Pool.Exec(ctx, `DO $$ BEGIN
        IF to_regclass('pending_object_deletions') IS NOT NULL THEN
            DELETE FROM pending_object_deletions;
        END IF;
    END $$`)
	return err
}

func (r *DocumentRepo) ObjectDeletionPending(ctx context.Context, sha string) (bool, error) {
	var pending bool
	err := r.db.Pool.QueryRow(ctx, `SELECT EXISTS (
        SELECT 1 FROM pending_object_deletions WHERE raw_sha256 = $1)`, sha).Scan(&pending)
	return pending, err
}

// ContainerDocuments walks the workspace's container documents in
// deterministic doc_id order, for the containment backfill.
//
// Containers are the document types the email worker unrolls — messages and
// archives — because they are the only things that produce children. Ordering
// by doc_id, stable once assigned, makes an interrupted walk resume over the
// same sequence rather than a re-shuffled one. A document with no Tier 1
// object is still returned, so the backfill can report it as unreadable
// instead of quietly leaving its containments underivable.
func (r *DocumentRepo) ContainerDocuments(ctx context.Context, after string, limit int) ([]domain.Document, error) {
	rows, err := r.db.Pool.Query(ctx, `
        SELECT d.doc_id::text, d.mime_type, d.rustfs_raw_uri, d.raw_sha256, d.source_filename
        FROM documents d
        WHERE d.doc_type IN ('email', 'archive')
          AND ($1::text = '' OR d.doc_id > $1::uuid)
        ORDER BY d.doc_id
        LIMIT $2`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list container documents: %w", err)
	}
	defer rows.Close()

	var out []domain.Document
	for rows.Next() {
		var d domain.Document
		if err := rows.Scan(&d.DocID, &d.MimeType, &d.RawURI, &d.RawSHA256, &d.SourceName); err != nil {
			return nil, fmt.Errorf("read container document: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Exists reports whether a document row is still present.
//
// Asked only on a failure path, where the question is whether the work failed
// or its subject was deliberately removed. A pruned document makes every
// downstream write fail — a chunk placement has nothing to reference — and
// those failures are not the work's fault and not worth a dead letter.
func (r *DocumentRepo) Exists(ctx context.Context, docID string) (bool, error) {
	var ok bool
	err := r.db.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM documents WHERE doc_id = $1)`, docID).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("check document %s: %w", docID, err)
	}
	return ok, nil
}

// HasPlacements reports whether a container already has recorded children.
//
// Asked of a container that cannot be unrolled, where it separates "this never
// held anything" from "this held something we can no longer enumerate". Only
// the second is a gap in the graph.
func (r *DocumentRepo) HasPlacements(ctx context.Context, containerID string) (bool, error) {
	var ok bool
	err := r.db.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM document_placements WHERE container_doc_id = $1)`,
		containerID).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("check placements for %s: %w", containerID, err)
	}
	return ok, nil
}
