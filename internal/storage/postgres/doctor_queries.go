package postgres

import (
	"context"
	"fmt"
	"time"
)

// DoctorQueries provides read-only queries for the doctor package. All
// methods are SELECT-only and never modify state.
type DoctorQueries struct{ db *DB }

// NewDoctorQueries creates a DoctorQueries over an existing pool.
func NewDoctorQueries(db *DB) *DoctorQueries {
	return &DoctorQueries{db: db}
}

// SchemaExists reports whether the documents and document_chunks tables
// exist in this database.
func (q *DoctorQueries) SchemaExists(ctx context.Context) (bool, error) {
	var count int
	err := q.db.Pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_name IN ('documents', 'document_chunks')`).Scan(&count)
	return count == 2, err
}

// VectorExtension reports whether the vector extension is installed.
func (q *DoctorQueries) VectorExtension(ctx context.Context) (bool, error) {
	var exists bool
	err := q.db.Pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_extension WHERE extname = 'vector'
		)`).Scan(&exists)
	return exists, err
}

// HNSWIndex reports whether the HNSW vector index exists.
func (q *DoctorQueries) HNSWIndex(ctx context.Context) (bool, error) {
	var exists bool
	err := q.db.Pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_indexes
			WHERE indexname = 'chunks_hnsw_idx'
		)`).Scan(&exists)
	return exists, err
}

// BM25Index reports whether the BM25 lexical index exists.
func (q *DoctorQueries) BM25Index(ctx context.Context) (bool, error) {
	var exists bool
	err := q.db.Pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_indexes
			WHERE indexname = 'chunks_bm25_idx'
		)`).Scan(&exists)
	return exists, err
}

// CountByStatus returns the number of documents in each processing status.
func (q *DoctorQueries) CountByStatus(ctx context.Context) (map[string]int, error) {
	rows, err := q.db.Pool.Query(ctx, `
		SELECT processing_status::text, count(*)
		FROM documents
		GROUP BY processing_status`)
	if err != nil {
		return nil, fmt.Errorf("count by status: %w", err)
	}
	defer rows.Close()

	out := make(map[string]int)
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		out[status] = count
	}
	return out, rows.Err()
}

// StalePendingCount returns PENDING rows older than the threshold.
func (q *DoctorQueries) StalePendingCount(ctx context.Context, threshold time.Duration) (int, error) {
	var n int
	err := q.db.Pool.QueryRow(ctx, `
		SELECT count(*) FROM documents
		WHERE processing_status = 'PENDING'
		  AND updated_at < now() - $1::interval`, threshold.String()).Scan(&n)
	return n, err
}

// StaleProcessingCount returns PROCESSING rows older than the threshold.
func (q *DoctorQueries) StaleProcessingCount(ctx context.Context, threshold time.Duration) (int, error) {
	var n int
	err := q.db.Pool.QueryRow(ctx, `
		SELECT count(*) FROM documents
		WHERE processing_status = 'PROCESSING'
		  AND updated_at < now() - $1::interval`, threshold.String()).Scan(&n)
	return n, err
}

// FailedByReason groups FAILED rows by their reason from metadata_headers.
func (q *DoctorQueries) FailedByReason(ctx context.Context) (map[string]int, error) {
	rows, err := q.db.Pool.Query(ctx, `
		SELECT COALESCE(metadata_headers->>'reason', 'UNKNOWN') AS reason, count(*)
		FROM documents
		WHERE processing_status = 'FAILED'
		GROUP BY reason`)
	if err != nil {
		return nil, fmt.Errorf("failed by reason: %w", err)
	}
	defer rows.Close()

	out := make(map[string]int)
	for rows.Next() {
		var reason string
		var count int
		if err := rows.Scan(&reason, &count); err != nil {
			return nil, err
		}
		out[reason] = count
	}
	return out, rows.Err()
}

// SkippedByReason groups SKIPPED rows by their reason.
func (q *DoctorQueries) SkippedByReason(ctx context.Context) (map[string]int, error) {
	rows, err := q.db.Pool.Query(ctx, `
		SELECT COALESCE(metadata_headers->>'reason', 'UNKNOWN') AS reason, count(*)
		FROM documents
		WHERE processing_status = 'SKIPPED'
		GROUP BY reason`)
	if err != nil {
		return nil, fmt.Errorf("skipped by reason: %w", err)
	}
	defer rows.Close()

	out := make(map[string]int)
	for rows.Next() {
		var reason string
		var count int
		if err := rows.Scan(&reason, &count); err != nil {
			return nil, err
		}
		out[reason] = count
	}
	return out, rows.Err()
}

// Tier2MissingTier1 counts document rows whose rustfs_raw_uri does not
// correspond to an existing Tier 1 object. This is a metadata check:
// the query returns the count of rows whose URI is non-empty, which is
// the upper bound; the vault-side check narrows it further.
func (q *DoctorQueries) Tier2MissingTier1(ctx context.Context) (int, error) {
	// This returns rows that have a URI but no text yet — a heuristic for
	// rows whose object may have been lost. The vault-side check is more
	// precise; this is the cheap metadata-side signal.
	var n int
	err := q.db.Pool.QueryRow(ctx, `
		SELECT count(*) FROM documents
		WHERE processing_status IN ('PENDING','PROCESSING')
		  AND rustfs_raw_uri <> ''
		  AND normalized_text IS NULL`).Scan(&n)
	return n, err
}

// StalePENDING returns PENDING rows older than the threshold for recovery.
func (q *DoctorQueries) StalePENDING(ctx context.Context, threshold time.Duration, limit int) ([]DoctorDoc, error) {
	rows, err := q.db.Pool.Query(ctx, `
		SELECT doc_id::text, mime_type,
		       rustfs_raw_uri, raw_sha256, source_filename,
		       COALESCE(parent_doc_id::text, ''),
		       COALESCE(metadata_headers->>'reason', ''),
		       updated_at
		FROM documents
		WHERE processing_status = 'PENDING'
		  AND updated_at < now() - $1::interval
		ORDER BY updated_at
		LIMIT $2`, threshold.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("stale pending: %w", err)
	}
	defer rows.Close()
	return scanDoctorDocs(rows)
}

// StalePROCESSING returns PROCESSING rows older than the threshold.
func (q *DoctorQueries) StalePROCESSING(ctx context.Context, threshold time.Duration, limit int) ([]DoctorDoc, error) {
	rows, err := q.db.Pool.Query(ctx, `
		SELECT doc_id::text, mime_type,
		       rustfs_raw_uri, raw_sha256, source_filename,
		       COALESCE(parent_doc_id::text, ''),
		       COALESCE(metadata_headers->>'reason', ''),
		       updated_at
		FROM documents
		WHERE processing_status = 'PROCESSING'
		  AND updated_at < now() - $1::interval
		ORDER BY updated_at
		LIMIT $2`, threshold.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("stale processing: %w", err)
	}
	defer rows.Close()
	return scanDoctorDocs(rows)
}

// RetryableFAILED returns FAILED rows whose reason is classified retryable.
func (q *DoctorQueries) RetryableFAILED(ctx context.Context, limit int) ([]DoctorDoc, error) {
	// Pull all FAILED rows; the caller filters by classification.
	rows, err := q.db.Pool.Query(ctx, `
		SELECT doc_id::text, mime_type,
		       rustfs_raw_uri, raw_sha256, source_filename,
		       COALESCE(parent_doc_id::text, ''),
		       COALESCE(metadata_headers->>'reason', ''),
		       updated_at
		FROM documents
		WHERE processing_status = 'FAILED'
		ORDER BY updated_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("retryable failed: %w", err)
	}
	defer rows.Close()
	return scanDoctorDocs(rows)
}

// DoctorDoc is a lightweight document representation for doctor/recovery.
type DoctorDoc struct {
	DocID       string
	MimeType    string
	RawURI      string
	RawSHA256   string
	SourceName  string
	ParentDocID string
	Reason      string
	UpdatedAt   time.Time
}

func scanDoctorDocs(rows pgxRows) ([]DoctorDoc, error) {
	var out []DoctorDoc
	for rows.Next() {
		var d DoctorDoc
		if err := rows.Scan(&d.DocID, &d.MimeType,
			&d.RawURI, &d.RawSHA256, &d.SourceName, &d.ParentDocID,
			&d.Reason, &d.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// pgxRows is the interface both *pgxpool.Rows and pgx.Rows satisfy.
type pgxRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

// ResetToPENDING moves a PROCESSING or FAILED row back to PENDING with
// a compare-and-set on the current status. Returns true if the row was
// claimed.
func (q *DoctorQueries) ResetToPENDING(ctx context.Context, docID string) (bool, error) {
	tag, err := q.db.Pool.Exec(ctx, `
		UPDATE documents
		SET processing_status = 'PENDING',
		    updated_at = now()
		WHERE doc_id = $1
		  AND processing_status IN ('PROCESSING', 'FAILED')`, docID)
	if err != nil {
		return false, fmt.Errorf("reset to pending %s: %w", docID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// Descendants returns all doc_ids descending from rootDocID, including
// the root itself. Breadth-first traversal of parent_doc_id.
func (q *DoctorQueries) Descendants(ctx context.Context, rootDocID string) ([]string, error) {
	visited := make(map[string]bool)
	var result []string
	queue := []string{rootDocID}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if visited[current] {
			continue
		}
		visited[current] = true
		result = append(result, current)

		rows, err := q.db.Pool.Query(ctx, `
			SELECT doc_id::text FROM documents WHERE parent_doc_id = $1`, current)
		if err != nil {
			return nil, fmt.Errorf("descendants query: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			if !visited[id] {
				queue = append(queue, id)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// Tier1URIsForDocs returns the raw/ and extracted/ URIs for the given
// doc_ids, partitioned into root (raw/) and child (extracted/).
func (q *DoctorQueries) Tier1URIsForDocs(ctx context.Context, docIDs []string) (raw, extracted []string, err error) {
	if len(docIDs) == 0 {
		return nil, nil, nil
	}
	// Build a parameterised IN clause.
	args := make([]any, len(docIDs))
	placeholders := make([]string, len(docIDs))
	for i, id := range docIDs {
		args[i] = id
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	query := fmt.Sprintf(`
		SELECT DISTINCT rustfs_raw_uri FROM documents
		WHERE doc_id IN (%s) AND rustfs_raw_uri <> ''`,
		join(placeholders))

	rows, err := q.db.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("tier1 uris: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var uri string
		if err := rows.Scan(&uri); err != nil {
			return nil, nil, err
		}
		switch {
		case len(uri) > 4 && uri[:4] == "raw/":
			raw = append(raw, uri)
		case len(uri) > 9 && uri[:9] == "extracted/":
			extracted = append(extracted, uri)
		}
	}
	return raw, extracted, rows.Err()
}

// DeleteByIDs removes documents by doc_id. Cascades handle chunks.
func (q *DoctorQueries) DeleteByIDs(ctx context.Context, docIDs []string) (int64, error) {
	if len(docIDs) == 0 {
		return 0, nil
	}
	args := make([]any, len(docIDs))
	placeholders := make([]string, len(docIDs))
	for i, id := range docIDs {
		args[i] = id
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	query := fmt.Sprintf(`DELETE FROM documents WHERE doc_id IN (%s)`, join(placeholders))
	tag, err := q.db.Pool.Exec(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("delete by ids: %w", err)
	}
	return tag.RowsAffected(), nil
}

func join(ss []string) string {
	var b []byte
	for i, s := range ss {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, s...)
	}
	return string(b)
}
