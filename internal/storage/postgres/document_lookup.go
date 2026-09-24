package postgres

import (
	"context"
	"fmt"
	"github.com/suankan/pocket-advisor/internal/source"
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
	SourceRef       string
	SourceLocations []source.Location
	DocID           string
	ParentDocID     string
	DocType         string
	MimeType        string
	SourceFilename  string
	EmailSubject    string
	EmailFrom       string
	EmailTo         string
	EmailDate       *time.Time
	NormalizedText  string
	SourcePath      string
	ContainerPath   string
}

// FetchDocumentMaxResults bounds every exact lookup below. These are point
// lookups on values a caller already has in hand (a sender+date, a subject
// the corpus actually contains, an exact filename), so a large match count
// signals an under-specified query rather than a browse that needs paging.
// Each lookup returns one row past the bound so the caller can say that more
// documents matched instead of silently dropping them.
const FetchDocumentMaxResults = 20

// documentsSelectSQL loads exact evidence; source locations are hydrated
// separately. The order is total and callers apply their bound after it, so
// a continuation that re-runs the same lookup meets the same matches in the
// same sequence.
const documentsSelectSQL = `
SELECT d.doc_id::text, COALESCE(d.parent_doc_id::text, ''), d.doc_type, d.mime_type,
       d.source_filename, d.email_subject, d.email_from, d.email_to, d.email_date,
       COALESCE(d.normalized_text, ''),
       COALESCE(d.metadata_headers->>'source_path', ''),
       ''::text
FROM documents d
JOIN origins o ON o.doc_id = d.doc_id
ORDER BY d.email_date DESC NULLS LAST, d.created_at DESC, d.doc_id`

// childrenSelectSQL is documentsSelectSQL without body text: a parent lists
// its children by identity and location, and a container can hold hundreds.
const childrenSelectSQL = `
SELECT d.doc_id::text, COALESCE(d.parent_doc_id::text, ''), d.doc_type, d.mime_type,
       d.source_filename, d.email_subject, d.email_from, d.email_to, d.email_date,
       ''::text,
       COALESCE(d.metadata_headers->>'source_path', ''),
       ''::text
FROM documents d
WHERE d.parent_doc_id = $1
ORDER BY d.email_date DESC NULLS LAST, d.created_at DESC, d.doc_id`

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
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out))
	for _, d := range out {
		ids = append(ids, d.DocID)
	}
	locations, err := NewSourceRepo(r.db).Locations(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].SourceRef = source.Ref(out[i].DocID)
		out[i].SourceLocations = locations[out[i].DocID]
		out[i].SourcePath, out[i].ContainerPath = source.DisplayLocations(out[i].SourceLocations)
	}
	return out, nil
}

// FindByFilename matches documents whose original filename, exactly as
// recorded at upload or extraction, equals filename. This is the only lookup
// mode that reaches attachments and non-email documents directly, since they
// carry no sender or subject of their own.
func (r *DocumentRepo) FindByFilename(ctx context.Context, filename string) ([]DocumentLocation, error) {
	return r.documentsFrom(ctx, `
        WITH RECURSIVE origins AS (
            SELECT doc_id FROM documents WHERE source_filename = $1
        )
        `+documentsSelectSQL+`
        LIMIT $2`, filename, FetchDocumentMaxResults+1)
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
        )
        `+documentsSelectSQL+`
        LIMIT $4`, sender, day, day.Add(24*time.Hour), FetchDocumentMaxResults+1)
}

// FindEmailsBySubjectDate matches email documents whose subject, exactly as
// stored, equals subject and that were sent on this UTC day.
func (r *DocumentRepo) FindEmailsBySubjectDate(ctx context.Context, subject string, day time.Time) ([]DocumentLocation, error) {
	return r.documentsFrom(ctx, `
        WITH RECURSIVE origins AS (
            SELECT doc_id FROM documents
            WHERE doc_type = 'email' AND email_subject = $1
              AND email_date >= $2 AND email_date < $3
        )
        `+documentsSelectSQL+`
        LIMIT $4`, subject, day, day.Add(24*time.Hour), FetchDocumentMaxResults+1)
}

// Children lists every direct descendant of one document — an email's
// attachments, or an office document's extracted parts — without their text.
// Ingestion's container expansion limits bound how many there are.
func (r *DocumentRepo) Children(ctx context.Context, docID string) ([]DocumentLocation, error) {
	return r.documentsFrom(ctx, childrenSelectSQL, docID)
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
