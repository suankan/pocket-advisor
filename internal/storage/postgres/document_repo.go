package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

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
	err = r.db.executor(ctx).QueryRow(ctx, `
        INSERT INTO documents (
            doc_id, parent_doc_id,
            processing_status, doc_type, mime_type, source_ref, raw_sha256,
            source_filename, metadata_headers)
        SELECT $1::uuid,$2::uuid,'PENDING',$3,$4,$5,$6::varchar,$7,$8::jsonb
        ON CONFLICT (raw_sha256) DO UPDATE SET raw_sha256 = EXCLUDED.raw_sha256
        RETURNING doc_id::text, (xmax = 0)`,
		d.DocID, parent,
		d.DocType, d.MimeType, "source:"+d.DocID, d.RawSHA256, d.SourceName, meta).Scan(&id, &inserted)

	if err != nil {
		return "", false, fmt.Errorf("create stub %s: %w", d.DocID, err)
	}
	return id, inserted, nil
}

// Status reads the current state of a document.
func (r *DocumentRepo) Status(ctx context.Context, docID string) (domain.Status, error) {
	var s string
	err := r.db.executor(ctx).QueryRow(ctx,
		`SELECT processing_status::text FROM documents WHERE doc_id = $1`, docID).Scan(&s)
	if err != nil {
		return "", err
	}
	return domain.Status(s), nil
}

// UpdateStatus moves a document to a terminal or intermediate state. reason is
// recorded in metadata_headers so SKIPPED and FAILED rows stay auditable.
func (r *DocumentRepo) UpdateStatus(ctx context.Context, docID string, s domain.Status, reason domain.FailureReason) error {
	_, err := r.db.executor(ctx).Exec(ctx, `
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
// ErrInvalidText marks text the canonical column cannot hold. PostgreSQL
// rejects a non-UTF-8 value at the driver level, as SQLSTATE 22021 naming a
// byte offset — unclassifiable by the runtime, so it retried three times and
// dead-lettered under UNCLASSIFIED, and on the admission path it ended the
// whole scan. Refused here instead, once, with a reason a caller can act on.
//
// Refused rather than transcoded: guessing an encoding produces valid UTF-8
// from wrong bytes, which is silent corruption of indexed evidence rather than
// a loud failure (engine/email.decodeText holds the same line).
var ErrInvalidText = errors.New("text is not valid UTF-8")

func (r *DocumentRepo) SaveText(ctx context.Context, docID, text, docType string) error {
	if !utf8.ValidString(text) {
		return fmt.Errorf("save text %s: %w", docID, ErrInvalidText)
	}
	_, err := r.db.executor(ctx).Exec(ctx, `
        UPDATE documents
        SET normalized_text = $2,
            extraction_hash = encode(sha256(convert_to($2,'UTF8')),'hex'),
            extraction_version = 'canonical-v1',
            doc_type        = COALESCE(NULLIF($3,''), doc_type),
            updated_at      = now()
        WHERE doc_id = $1`, docID, text, docType)
	if err != nil {
		return fmt.Errorf("save text %s: %w", docID, err)
	}
	return nil
}

// SaveEmailText writes an email's body together with the RFC822 headers
// promoted out of it. The headers become columns rather than staying inline in
// normalized_text: they are metadata about a message, not prose the author
// wrote into it, and they repeat identically across every reply.
// Nothing re-attaches them to the indexed text — what a chunk belongs to is a
// retrieval-time lookup, not part of its vector (§5.3, §5.6).
func (r *DocumentRepo) SaveEmailText(
	ctx context.Context, docID, text string, h domain.EmailHeaders,
) error {
	if !utf8.ValidString(text) {
		return fmt.Errorf("save email text %s: %w", docID, ErrInvalidText)
	}
	var date any
	if !h.Date.IsZero() {
		date = h.Date
	}
	_, err := r.db.executor(ctx).Exec(ctx, `
        UPDATE documents
        SET normalized_text = $2,
            extraction_hash = encode(sha256(convert_to($2,'UTF8')),'hex'),
            extraction_version = 'canonical-v1',
            doc_type        = 'email',
            email_subject   = $3,
            email_from      = $4,
            email_to        = $5,
            email_date      = $6,
            updated_at      = now()
        WHERE doc_id = $1`,
		docID, text, h.Subject, h.From, h.To, date)
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
	err := r.db.executor(ctx).QueryRow(ctx,
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
	rows, err := r.db.executor(ctx).Query(ctx, `
        SELECT doc_id::text, mime_type,
               source_ref, raw_sha256, source_filename,
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
			&d.SourceRef, &d.RawSHA256, &d.SourceName, &d.ParentDocID); err != nil {
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
	err := r.db.executor(ctx).QueryRow(ctx, `
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
	rows, err := r.db.executor(ctx).Query(ctx, `
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

// DeleteWorkspace empties this workspace's database: every document, with
// chunks cascading. This is the Tier 2 half of --delete-data, which must leave
// no derived row describing content the workspace no longer indexes (§5.1).
func (r *DocumentRepo) DeleteWorkspace(ctx context.Context) (int64, error) {
	var n int64
	err := r.db.Transaction(ctx, func(ctx context.Context) error {
		tag, err := r.db.executor(ctx).Exec(ctx, "DELETE FROM documents")
		if err != nil {
			return err
		}
		n = tag.RowsAffected()
		for _, table := range []string{"chunks", "source_scans", "source_roots"} {
			if _, err := r.db.executor(ctx).Exec(ctx, "DELETE FROM "+table); err != nil {
				return err
			}
		}
		return nil
	})
	return n, err
}

// DeleteBySHA forgets the selected document and exclusively held descendants.
// Other roots and containers retain shared content, including nested children.
func (r *DocumentRepo) DeleteBySHA(ctx context.Context, sha string) (int64, error) {
	tx, err := r.db.executor(ctx).Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
        WITH RECURSIVE target AS (
            SELECT doc_id FROM documents WHERE raw_sha256 = $1
        ), descendants AS (
            SELECT doc_id FROM target
            UNION
            SELECT p.contained_doc_id FROM document_placements p
            JOIN descendants d ON p.container_doc_id = d.doc_id
        ), retained AS (
            SELECT d.doc_id FROM documents d
            WHERE NOT EXISTS (SELECT 1 FROM target t WHERE t.doc_id = d.doc_id)
              AND (NOT EXISTS (SELECT 1 FROM descendants s WHERE s.doc_id = d.doc_id)
                   OR COALESCE(d.metadata_headers->>'source_path', '') <> '')
            UNION
            SELECT p.contained_doc_id FROM document_placements p
            JOIN retained r ON p.container_doc_id = r.doc_id
            WHERE NOT EXISTS (SELECT 1 FROM target t WHERE t.doc_id = p.contained_doc_id)
        )
        SELECT d.doc_id::text FROM descendants d
        WHERE NOT EXISTS (SELECT 1 FROM retained r WHERE r.doc_id = d.doc_id)`, sha)
	if err != nil {
		return 0, fmt.Errorf("plan forget: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	return deleteDocuments(ctx, tx, ids)
}

// UpdateSourcePath refreshes the recorded source location for the document
// with this content hash. Identity here is raw_sha256, never doc_id or the
// old path (reconcile.go): a document's source_path is set once, at first
// admission, and otherwise never revisited, so a same-content move leaves it
// pointing at a file that no longer exists until this is called explicitly
// (the scan does so when it finds existing content at a new path).
func (r *DocumentRepo) UpdateSourcePath(ctx context.Context, sha, path string) error {
	_, err := r.db.executor(ctx).Exec(ctx, `
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

// ResolveContentHashes maps raw content hashes to this database's document
// ids.
//
// A hash with no row is simply absent from the result rather than an error:
// "this workspace does not hold that content" is a legitimate answer, and the
// caller is better placed to decide whether it is a problem. raw_sha256
// carries a UNIQUE constraint, so each present hash yields exactly one id.
func (r *DocumentRepo) ResolveContentHashes(ctx context.Context, hashes []string) (map[string]string, error) {
	out := make(map[string]string, len(hashes))
	if len(hashes) == 0 {
		return out, nil
	}
	rows, err := r.db.executor(ctx).Query(ctx,
		`SELECT raw_sha256, doc_id::text FROM documents WHERE raw_sha256 = ANY($1::text[])`, hashes)
	if err != nil {
		return nil, fmt.Errorf("resolve content hashes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var hash, id string
		if err := rows.Scan(&hash, &id); err != nil {
			return nil, err
		}
		out[hash] = id
	}
	return out, rows.Err()
}
