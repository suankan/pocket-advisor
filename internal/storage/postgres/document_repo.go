package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/suankan/pocket-advisor/internal/domain"
)

type DocumentRepo struct{ db *DB }

func NewDocumentRepo(db *DB) *DocumentRepo { return &DocumentRepo{db: db} }

// CreateStub inserts a PENDING row, or finds the existing row for the same
// content, and reports the resolved doc_id plus whether it was newly
// created. d.DocID is only a candidate: doc_id and raw_sha256 are
// deliberately independent (schema.go's documents_raw_sha256_key), so this
// upserts on the content hash, not on the candidate id, and the id actually
// stored — d.DocID's value on a fresh insert, or whichever id an earlier
// insert of the same bytes already claimed — is what every caller must use
// from here on, not d.DocID itself. A duplicate is an expected outcome of
// the idempotent entry path, not a failure — hence a bool rather than an
// error (§8.2).
func (r *DocumentRepo) CreateStub(ctx context.Context, d *domain.Document) (string, bool, error) {
	meta, err := json.Marshal(orEmpty(d.Metadata))
	if err != nil {
		return "", false, fmt.Errorf("marshal metadata: %w", err)
	}

	var parent any
	if d.ParentDocID != "" {
		parent = d.ParentDocID
	}

	var id string
	var inserted bool
	err = r.db.Pool.QueryRow(ctx, `
        INSERT INTO documents (
            doc_id, parent_doc_id, thread_id,
            processing_status, doc_type, mime_type, rustfs_raw_uri, raw_sha256,
            source_filename, metadata_headers)
        VALUES ($1,$2,$3,'PENDING',$4,$5,$6,$7,$8,$9)
        ON CONFLICT (raw_sha256) DO UPDATE SET raw_sha256 = EXCLUDED.raw_sha256
        RETURNING doc_id::text, (xmax = 0)`,
		d.DocID, parent, d.ThreadID,
		d.DocType, d.MimeType, d.RawURI, d.RawSHA256, d.SourceName, meta).Scan(&id, &inserted)

	if err != nil {
		return "", false, fmt.Errorf("create stub %s: %w", d.DocID, err)
	}
	return id, inserted, nil
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
func (r *DocumentRepo) UpdateStatus(ctx context.Context, docID string, s domain.Status, reason domain.FailureReason) error {
	_, err := r.db.Pool.Exec(ctx, `
        UPDATE documents
        SET processing_status = $2::processing_status,
            metadata_headers  = CASE WHEN $3 = '' THEN metadata_headers
                                     ELSE metadata_headers || jsonb_build_object('reason', $3) END,
            updated_at        = now()
        WHERE doc_id = $1`, docID, string(s), string(reason))
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

// SaveEmailText writes an email's body together with the RFC822 headers
// promoted out of it. The headers become columns rather than staying inline in
// normalized_text: they are metadata about a message, not prose the author
// wrote into it, and they repeat identically across every message in a thread.
// Nothing re-attaches them to the indexed text — what a chunk belongs to is a
// retrieval-time lookup, not part of its vector (§5.3, §5.6).
func (r *DocumentRepo) SaveEmailText(
	ctx context.Context, docID, text, threadID string, h domain.EmailHeaders,
) error {
	var date any
	if !h.Date.IsZero() {
		date = h.Date
	}
	_, err := r.db.Pool.Exec(ctx, `
        UPDATE documents
        SET normalized_text = $2,
            doc_type        = 'email',
            thread_id       = COALESCE(NULLIF($3,''), thread_id),
            email_subject   = $4,
            email_from      = $5,
            email_to        = $6,
            email_date      = $7,
            updated_at      = now()
        WHERE doc_id = $1`,
		docID, text, threadID, h.Subject, h.From, h.To, date)
	if err != nil {
		return fmt.Errorf("save email text %s: %w", docID, err)
	}
	return nil
}

// LoadedText is what the embedding indexer needs to chunk a document.
// Deliberately just the text: a chunk carries nothing borrowed from the
// document it came from (§5.6).
type LoadedText struct {
	Text string
}

// LoadText reads normalized_text back for the embedding indexer.
func (r *DocumentRepo) LoadText(ctx context.Context, docID string) (LoadedText, error) {
	var text *string
	err := r.db.Pool.QueryRow(ctx,
		`SELECT normalized_text FROM documents WHERE doc_id = $1`,
		docID).Scan(&text)
	if err != nil {
		return LoadedText{}, fmt.Errorf("load text %s: %w", docID, err)
	}
	return LoadedText{Text: deref(text)}, nil
}

// ClaimStalePending returns documents stuck PENDING past the threshold, for
// the reconciliation sweep (§2.2).
func (r *DocumentRepo) ClaimStalePending(ctx context.Context, olderThan time.Duration, limit int) ([]domain.Document, error) {
	rows, err := r.db.Pool.Query(ctx, `
        SELECT doc_id::text, mime_type,
               rustfs_raw_uri, raw_sha256, source_filename,
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
		if err := rows.Scan(&d.DocID, &d.MimeType,
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

// StagedRoot is one root document that records where it was staged from.
// Only these can be judged against the staging directory: a child created by
// a container worker was never a file there, and is removed with its root.
type StagedRoot struct {
	DocID       string
	SHA256      string
	SourcePath  string
	DocType     string
	Descendants int
}

// StagedRoots lists every root document carrying a staged source path,
// with the number of documents that would be removed along with it.
//
// The descendant count is reported rather than assumed: deleting one staged
// file can remove a whole extracted tree, and an operator confirming a
// deletion needs to see that before agreeing to it, not after.
func (r *DocumentRepo) StagedRoots(ctx context.Context) ([]StagedRoot, error) {
	rows, err := r.db.Pool.Query(ctx, `
        WITH RECURSIVE tree AS (
            SELECT doc_id AS root, doc_id
            FROM documents
            WHERE parent_doc_id IS NULL
              AND NULLIF(metadata_headers->>'source_path', '') IS NOT NULL
            UNION ALL
            SELECT t.root, d.doc_id
            FROM documents d JOIN tree t ON d.parent_doc_id = t.doc_id
        ),
        sizes AS (
            SELECT root, count(*) - 1 AS descendants FROM tree GROUP BY root
        )
        SELECT d.doc_id::text, d.raw_sha256,
               d.metadata_headers->>'source_path', d.doc_type,
               COALESCE(s.descendants, 0)
        FROM documents d
        LEFT JOIN sizes s ON s.root = d.doc_id
        WHERE d.parent_doc_id IS NULL
          AND NULLIF(d.metadata_headers->>'source_path', '') IS NOT NULL
        ORDER BY d.metadata_headers->>'source_path'`)
	if err != nil {
		return nil, fmt.Errorf("list staged roots: %w", err)
	}
	defer rows.Close()

	var out []StagedRoot
	for rows.Next() {
		var s StagedRoot
		if err := rows.Scan(&s.DocID, &s.SHA256, &s.SourcePath, &s.DocType, &s.Descendants); err != nil {
			return nil, fmt.Errorf("read staged root: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// KnownRawURIs returns the set of Tier 1 URIs already represented in Tier 2,
// for the bucket-scan anti-join (§5.2).
func (r *DocumentRepo) KnownRawURIs(ctx context.Context) (map[string]struct{}, error) {
	rows, err := r.db.Pool.Query(ctx,
		`SELECT rustfs_raw_uri FROM documents WHERE rustfs_raw_uri <> ''`)
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

// DeleteWorkspace empties this workspace's database: every document, with
// chunks cascading. This is the Tier 2 half of `uploader --wipe`, which must
// not leave the database populated against an emptied bucket (§5.1).
func (r *DocumentRepo) DeleteWorkspace(ctx context.Context) (int64, error) {
	tag, err := r.db.Pool.Exec(ctx, `DELETE FROM documents`)
	if err != nil {
		return 0, fmt.Errorf("delete workspace: %w", err)
	}
	// The identifier graph does not cascade: its nodes outlive the documents
	// they name, which is exactly how a conversation survives a missing
	// ancestor. That makes them the one thing a wipe would leave behind — a
	// graph of placeholders for a corpus that no longer exists — so they are
	// removed explicitly (§2.5).
	if _, err := r.db.Pool.Exec(ctx, `DELETE FROM email_identifier_nodes`); err != nil {
		return tag.RowsAffected(), fmt.Errorf("delete workspace identifier graph: %w", err)
	}
	return tag.RowsAffected(), nil
}

// DeleteBySHA removes one document and its descendants, for --forget.
func (r *DocumentRepo) DeleteBySHA(ctx context.Context, sha string) (int64, error) {
	tag, err := r.db.Pool.Exec(ctx, `DELETE FROM documents WHERE raw_sha256 = $1`, sha)
	if err != nil {
		return 0, fmt.Errorf("delete %s: %w", sha, err)
	}
	return tag.RowsAffected(), nil
}

// UpdateSourcePath refreshes the recorded staging location for the document
// with this content hash. Identity here is raw_sha256, never doc_id or the
// old path (reconcile.go): a document's source_path is set once, at first
// upload, and otherwise never revisited, so a same-content move leaves it
// pointing at a file that no longer exists until this is called explicitly
// (the uploader does so when it finds existing content at a new path).
func (r *DocumentRepo) UpdateSourcePath(ctx context.Context, sha, path string) error {
	_, err := r.db.Pool.Exec(ctx, `
        UPDATE documents
        SET metadata_headers = jsonb_set(metadata_headers, '{source_path}', to_jsonb($2::text)),
            updated_at       = now()
        WHERE raw_sha256 = $1`, sha, path)
	if err != nil {
		return fmt.Errorf("update source path %s: %w", sha, err)
	}
	return nil
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
