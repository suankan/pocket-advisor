package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/suankan/pocket-advisor/internal/domain"
)

type DocumentRepo struct{ db *DB }

func NewDocumentRepo(db *DB) *DocumentRepo { return &DocumentRepo{db: db} }

// CreateStub inserts a PENDING row, reporting whether it was newly created.
// A duplicate is an expected outcome of the idempotent entry path, not a
// failure — hence a bool rather than an error (§8.2).
func (r *DocumentRepo) CreateStub(ctx context.Context, d *domain.Document) (bool, error) {
	meta, err := json.Marshal(orEmpty(d.Metadata))
	if err != nil {
		return false, fmt.Errorf("marshal metadata: %w", err)
	}

	var parent any
	if d.ParentDocID != "" {
		parent = d.ParentDocID
	}

	var id string
	err = r.db.Pool.QueryRow(ctx, `
        INSERT INTO documents (
            doc_id, parent_doc_id, workspace_id, collection_id, thread_id,
            processing_status, doc_type, mime_type, minio_raw_uri, raw_sha256,
            source_filename, metadata_headers)
        VALUES ($1,$2,$3,$4,$5,'PENDING',$6,$7,$8,$9,$10,$11)
        ON CONFLICT (doc_id) DO NOTHING
        RETURNING doc_id::text`,
		d.DocID, parent, d.WorkspaceID, d.Collection, d.ThreadID,
		d.DocType, d.MimeType, d.RawURI, d.RawSHA256, d.SourceName, meta).Scan(&id)

	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // already known
	}
	if err != nil {
		return false, fmt.Errorf("create stub %s: %w", d.DocID, err)
	}
	return true, nil
}

// Status reads the current state of a document.
func (r *DocumentRepo) Status(ctx context.Context, docID string) (domain.Status, error) {
	var s string
	err := r.db.Pool.QueryRow(ctx,
		`SELECT processing_status::text FROM documents WHERE doc_id = $1`, docID).Scan(&s)
	if err != nil {
		return "", err
	}
	return domain.Status(s), nil
}

// UpdateStatus moves a document to a terminal or intermediate state. reason is
// recorded in metadata_headers so SKIPPED and FAILED rows stay auditable.
func (r *DocumentRepo) UpdateStatus(ctx context.Context, docID string, s domain.Status, reason string) error {
	_, err := r.db.Pool.Exec(ctx, `
        UPDATE documents
        SET processing_status = $2::processing_status,
            metadata_headers  = CASE WHEN $3 = '' THEN metadata_headers
                                     ELSE metadata_headers || jsonb_build_object('reason', $3) END,
            updated_at        = now()
        WHERE doc_id = $1`, docID, string(s), reason)
	if err != nil {
		return fmt.Errorf("update status %s: %w", docID, err)
	}
	return nil
}

// SaveText writes normalized_text. The extractor that produced the text owns
// this column; commands carry a doc_id reference rather than the text itself
// (§4.1).
func (r *DocumentRepo) SaveText(ctx context.Context, docID, text, docType, threadID string) error {
	_, err := r.db.Pool.Exec(ctx, `
        UPDATE documents
        SET normalized_text = $2,
            doc_type        = COALESCE(NULLIF($3,''), doc_type),
            thread_id       = COALESCE(NULLIF($4,''), thread_id),
            updated_at      = now()
        WHERE doc_id = $1`, docID, text, docType, threadID)
	if err != nil {
		return fmt.Errorf("save text %s: %w", docID, err)
	}
	return nil
}

// LoadText reads normalized_text back for the embedding indexer.
func (r *DocumentRepo) LoadText(ctx context.Context, docID string) (string, string, error) {
	var text, workspace *string
	err := r.db.Pool.QueryRow(ctx,
		`SELECT normalized_text, workspace_id FROM documents WHERE doc_id = $1`,
		docID).Scan(&text, &workspace)
	if err != nil {
		return "", "", fmt.Errorf("load text %s: %w", docID, err)
	}
	return deref(text), deref(workspace), nil
}

// ClaimStalePending returns documents stuck PENDING past the threshold, for
// the reconciliation sweep (§2.2).
func (r *DocumentRepo) ClaimStalePending(ctx context.Context, olderThan time.Duration, limit int) ([]domain.Document, error) {
	rows, err := r.db.Pool.Query(ctx, `
        SELECT doc_id::text, workspace_id, collection_id, mime_type,
               minio_raw_uri, raw_sha256, source_filename,
               COALESCE(parent_doc_id::text, '')
        FROM documents
        WHERE processing_status = 'PENDING'
          AND updated_at < now() - $1::interval
        ORDER BY updated_at
        LIMIT $2`, olderThan.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("claim stale pending: %w", err)
	}
	defer rows.Close()

	var out []domain.Document
	for rows.Next() {
		var d domain.Document
		if err := rows.Scan(&d.DocID, &d.WorkspaceID, &d.Collection, &d.MimeType,
			&d.RawURI, &d.RawSHA256, &d.SourceName, &d.ParentDocID); err != nil {
			return nil, err
		}
		d.Status = domain.StatusPending
		out = append(out, d)
	}
	return out, rows.Err()
}

// CountStalePending backs the rag_discovery_stale_pending gauge.
func (r *DocumentRepo) CountStalePending(ctx context.Context, olderThan time.Duration) (int, error) {
	var n int
	err := r.db.Pool.QueryRow(ctx, `
        SELECT count(*) FROM documents
        WHERE processing_status = 'PENDING' AND updated_at < now() - $1::interval`,
		olderThan.String()).Scan(&n)
	return n, err
}

// KnownRawURIs returns the set of Tier 1 URIs already represented in Tier 2,
// for the bucket-scan anti-join (§5.2).
func (r *DocumentRepo) KnownRawURIs(ctx context.Context, workspaceID string) (map[string]struct{}, error) {
	rows, err := r.db.Pool.Query(ctx,
		`SELECT minio_raw_uri FROM documents WHERE workspace_id = $1 AND minio_raw_uri <> ''`,
		workspaceID)
	if err != nil {
		return nil, fmt.Errorf("known raw uris: %w", err)
	}
	defer rows.Close()

	out := make(map[string]struct{})
	for rows.Next() {
		var uri string
		if err := rows.Scan(&uri); err != nil {
			return nil, err
		}
		out[uri] = struct{}{}
	}
	return out, rows.Err()
}

// DeleteWorkspace removes every document in a workspace; chunks cascade.
// This is the Tier 2 half of `uploader --wipe`, which must not leave the
// database populated against an emptied bucket (§5.1).
func (r *DocumentRepo) DeleteWorkspace(ctx context.Context, workspaceID string) (int64, error) {
	tag, err := r.db.Pool.Exec(ctx, `DELETE FROM documents WHERE workspace_id = $1`, workspaceID)
	if err != nil {
		return 0, fmt.Errorf("delete workspace %s: %w", workspaceID, err)
	}
	return tag.RowsAffected(), nil
}

// DeleteBySHA removes one document and its descendants, for --forget.
func (r *DocumentRepo) DeleteBySHA(ctx context.Context, workspaceID, sha string) (int64, error) {
	tag, err := r.db.Pool.Exec(ctx,
		`DELETE FROM documents WHERE workspace_id = $1 AND raw_sha256 = $2`, workspaceID, sha)
	if err != nil {
		return 0, fmt.Errorf("delete %s: %w", sha, err)
	}
	return tag.RowsAffected(), nil
}

func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
