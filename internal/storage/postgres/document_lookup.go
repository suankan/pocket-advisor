package postgres

import (
	"context"
	"fmt"
	"time"
)

// DocumentLocation is one document as a deterministic exact-attribute lookup
// (fetch_document) reports it: enough to identify, locate, and — for an
// email — attribute it, plus its body text.
//
// SourcePath and ContainerPath follow the same rule the read path applies
// everywhere else: a document either was its own staged file or lives inside
// one that was, never both, and the container is never inferred from where
// sibling documents happen to live (ingestion-design.md, StagedRoots's
// sibling in this file).
type DocumentLocation struct {
	DocID          string
	ParentDocID    string
	DocType        string
	MimeType       string
	SourceFilename string
	EmailSubject   string
	EmailFrom      string
	EmailTo        string
	EmailDate      *time.Time
	NormalizedText string
	SourcePath     string
	ContainerPath  string
}

// fetchDocumentMaxResults bounds every exact lookup below. These are point
// lookups on values a caller already has in hand (a sender+date, a subject
// the corpus actually contains, an exact filename), so a large match count
// signals an under-specified query rather than a browse that needs paging.
const fetchDocumentMaxResults = 20

// documentAncestryAndSelectSQL resolves the container ancestry for whatever
// origins CTE the caller prepends, exactly as retrieval's containerAncestrySQL
// does (internal/retrieval/expand.go) — duplicated rather than shared because
// each read path's SQL is that path's own query plan, not a general-purpose
// repository (the same call the mailbox store's package doc makes for its
// own source-path hydration).
const documentAncestryAndSelectSQL = `
ancestry AS (
    SELECT DISTINCT origin.doc_id AS origin, source.parent_doc_id,
           NULLIF(source.metadata_headers->>'source_path', '') AS source_path, 0 AS hops
    FROM origins origin
    JOIN documents source ON source.doc_id = origin.doc_id
    UNION ALL
    SELECT walk.origin, parent.parent_doc_id,
           NULLIF(parent.metadata_headers->>'source_path', ''), walk.hops + 1
    FROM documents parent
    JOIN ancestry walk ON walk.parent_doc_id = parent.doc_id
    WHERE walk.source_path IS NULL AND walk.hops < 16
),
container AS (
    SELECT DISTINCT ON (origin) origin, source_path
    FROM ancestry
    WHERE source_path IS NOT NULL AND hops > 0
    ORDER BY origin, hops
)
SELECT d.doc_id::text, COALESCE(d.parent_doc_id::text, ''), d.doc_type, d.mime_type,
       d.source_filename, d.email_subject, d.email_from, d.email_to, d.email_date,
       COALESCE(d.normalized_text, ''),
       COALESCE(d.metadata_headers->>'source_path', ''),
       COALESCE(c.source_path, '')
FROM documents d
JOIN origins o ON o.doc_id = d.doc_id
LEFT JOIN container c ON c.origin = d.doc_id
ORDER BY d.email_date DESC NULLS LAST, d.created_at DESC`

func (r *DocumentRepo) documentsFrom(ctx context.Context, query string, args ...any) ([]DocumentLocation, error) {
	rows, err := r.db.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("find documents: %w", err)
	}
	defer rows.Close()

	var out []DocumentLocation
	for rows.Next() {
		var d DocumentLocation
		if err := rows.Scan(&d.DocID, &d.ParentDocID, &d.DocType, &d.MimeType,
			&d.SourceFilename, &d.EmailSubject, &d.EmailFrom, &d.EmailTo, &d.EmailDate,
			&d.NormalizedText, &d.SourcePath, &d.ContainerPath); err != nil {
			return nil, fmt.Errorf("read document: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// FindByFilename matches documents whose original filename, exactly as
// recorded at upload or extraction, equals filename. This is the only lookup
// mode that reaches attachments and non-email documents directly, since they
// carry no sender or subject of their own.
func (r *DocumentRepo) FindByFilename(ctx context.Context, filename string) ([]DocumentLocation, error) {
	return r.documentsFrom(ctx, `
        WITH RECURSIVE origins AS (
            SELECT doc_id FROM documents WHERE source_filename = $1 LIMIT $2
        ),
        `+documentAncestryAndSelectSQL, filename, fetchDocumentMaxResults)
}

// FindEmailsBySenderDate matches email documents from exactly this sender
// mailbox (normalized, as stored in email_addresses) sent on this UTC day.
func (r *DocumentRepo) FindEmailsBySenderDate(ctx context.Context, sender string, day time.Time) ([]DocumentLocation, error) {
	return r.documentsFrom(ctx, `
        WITH RECURSIVE origins AS (
            SELECT DISTINCT d.doc_id
            FROM documents d
            JOIN email_addresses a ON a.doc_id = d.doc_id AND a.kind = 'from' AND a.valid
            WHERE a.address = $1 AND d.email_date >= $2 AND d.email_date < $3
            LIMIT $4
        ),
        `+documentAncestryAndSelectSQL, sender, day, day.Add(24*time.Hour), fetchDocumentMaxResults)
}

// FindEmailsBySubjectDate matches email documents whose subject, exactly as
// stored, equals subject and that were sent on this UTC day.
func (r *DocumentRepo) FindEmailsBySubjectDate(ctx context.Context, subject string, day time.Time) ([]DocumentLocation, error) {
	return r.documentsFrom(ctx, `
        WITH RECURSIVE origins AS (
            SELECT doc_id FROM documents
            WHERE doc_type = 'email' AND email_subject = $1
              AND email_date >= $2 AND email_date < $3
            LIMIT $4
        ),
        `+documentAncestryAndSelectSQL, subject, day, day.Add(24*time.Hour), fetchDocumentMaxResults)
}

// Children lists the direct descendants of one document — an email's
// attachments, or an office document's extracted parts.
func (r *DocumentRepo) Children(ctx context.Context, docID string) ([]DocumentLocation, error) {
	return r.documentsFrom(ctx, `
        WITH RECURSIVE origins AS (
            SELECT doc_id FROM documents WHERE parent_doc_id = $1 LIMIT $2
        ),
        `+documentAncestryAndSelectSQL, docID, fetchDocumentMaxResults)
}

// EmailRecipients splits one email's To and Cc addresses, exactly as
// recorded at ingestion. Bcc is never returned: a document in this corpus was
// never a Bcc recipient's own copy revealing who else was blind-copied.
func (r *DocumentRepo) EmailRecipients(ctx context.Context, docID string) (to, cc []string, err error) {
	rows, err := r.db.Pool.Query(ctx, `
        SELECT kind, address FROM email_addresses
        WHERE doc_id = $1 AND valid AND kind IN ('to','cc')
        ORDER BY kind, ordinal`, docID)
	if err != nil {
		return nil, nil, fmt.Errorf("read recipients %s: %w", docID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var kind, address string
		if err := rows.Scan(&kind, &address); err != nil {
			return nil, nil, fmt.Errorf("read recipient: %w", err)
		}
		if kind == "to" {
			to = append(to, address)
		} else {
			cc = append(cc, address)
		}
	}
	return to, cc, rows.Err()
}
