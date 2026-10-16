package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/suankan/pocket-advisor/internal/domain"
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

// liveLease excludes a document whose job holds an unexpired lease: its handler
// is working, however old the row's timestamp is. A handler's own status writes
// belong to its open transaction, so a document being processed often still
// reads PENDING.
const liveLease = ` AND NOT EXISTS (SELECT 1 FROM ingestion_jobs j WHERE j.doc_id = documents.doc_id
		    AND j.state = 'RUNNING' AND j.lease_until > clock_timestamp())`

// StalePendingCount returns PENDING rows older than the threshold. With
// excludeLive, a document whose job holds an unexpired lease is not stale. Pass
// false on the pre-upgrade layout, whose job table has no leases.
func (q *DoctorQueries) StalePendingCount(ctx context.Context, threshold time.Duration, excludeLive bool) (int, error) {
	var n int
	live := ""
	if excludeLive {
		live = liveLease
	}
	err := q.db.Pool.QueryRow(ctx, `
		SELECT count(*) FROM documents
		WHERE processing_status = 'PENDING'
		  AND updated_at < now() - $1::interval`+live, threshold.String()).Scan(&n)
	return n, err
}

// StaleProcessingCount returns PROCESSING rows older than the threshold. With
// excludeLive, a document whose job holds an unexpired lease is not stale: its
// handler is working, however old the row's timestamp is. Pass false on the
// pre-upgrade layout, whose job table has no leases.
func (q *DoctorQueries) StaleProcessingCount(ctx context.Context, threshold time.Duration, excludeLive bool) (int, error) {
	var n int
	live := ""
	if excludeLive {
		live = liveLease
	}
	err := q.db.Pool.QueryRow(ctx, `
		SELECT count(*) FROM documents
		WHERE processing_status = 'PROCESSING'
		  AND updated_at < now() - $1::interval`+live, threshold.String()).Scan(&n)
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

// NamedDocs names the documents behind one status and reason.
type NamedDocs struct {
	// Jobless counts those with no job at all: recorded before the job queue.
	Jobless int
	// Locations name them as the run summary does, most recently updated
	// first, at most as many as were asked for.
	Locations []string
}

// DocumentsByReason names the documents with status, by reason as
// FailedByReason and SkippedByReason key it, at most limit per reason, or all
// of them when limit is 0. When reasons is not empty, only those are named.
// It reads the job table, so it needs the current queue layout.
func (q *DoctorQueries) DocumentsByReason(ctx context.Context, status domain.Status, reasons []string, limit int) (map[string]NamedDocs, error) {
	const picked = `WITH picked AS (
   SELECT doc_id, COALESCE(metadata_headers->>'reason', 'UNKNOWN') AS reason,
          row_number() OVER (PARTITION BY metadata_headers->>'reason' ORDER BY updated_at DESC, doc_id) AS n,
          NOT EXISTS (SELECT 1 FROM ingestion_jobs j WHERE j.doc_id = documents.doc_id) AS jobless
   FROM documents
   WHERE processing_status = $1
     AND (cardinality($2::text[]) = 0 OR metadata_headers->>'reason' = ANY($2::text[])))`
	if reasons == nil {
		reasons = []string{}
	}
	out := map[string]NamedDocs{}
	rows, err := q.db.Pool.Query(ctx, picked+`
 SELECT reason, count(*) FILTER (WHERE jobless) FROM picked GROUP BY reason`, string(status), reasons)
	if err != nil {
		return nil, fmt.Errorf("documents by reason: %w", err)
	}
	for rows.Next() {
		var reason string
		var named NamedDocs
		if err := rows.Scan(&reason, &named.Jobless); err != nil {
			rows.Close()
			return nil, err
		}
		out[reason] = named
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = q.db.Pool.Query(ctx, picked+`
 SELECT p.reason, `+locationColumns+`
 FROM picked p JOIN documents d USING (doc_id)
 `+locationJoin+`
 WHERE $3 = 0 OR p.n <= $3
 ORDER BY p.reason, p.n`, string(status), reasons, limit)
	if err != nil {
		return nil, fmt.Errorf("documents by reason: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var reason, name, own, containerPath, containerName string
		if err := rows.Scan(&reason, &name, &own, &containerPath, &containerName); err != nil {
			return nil, err
		}
		named := out[reason]
		named.Locations = append(named.Locations, location(name, own, containerPath, containerName))
		out[reason] = named
	}
	return out, rows.Err()
}

// DocumentsWithoutSources counts inventory gaps, not live filesystem probes.
func (q *DoctorQueries) DocumentsWithoutSources(ctx context.Context) (int, error) {
	var n int
	err := q.db.Pool.QueryRow(ctx, `WITH RECURSIVE reachable AS (
 SELECT doc_id FROM source_occurrences WHERE available
 UNION SELECT m.child_id FROM source_members m JOIN reachable r ON r.doc_id=m.parent_id)
 SELECT count(*) FROM documents d WHERE NOT EXISTS(SELECT 1 FROM reachable r WHERE r.doc_id=d.doc_id)`).Scan(&n)
	return n, err
}

// StalePENDING returns PENDING rows older than the threshold for recovery.
func (q *DoctorQueries) StalePENDING(ctx context.Context, threshold time.Duration, limit int) ([]DoctorDoc, error) {
	rows, err := q.db.Pool.Query(ctx, `
		SELECT `+doctorDocColumns+`
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
		SELECT `+doctorDocColumns+`
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

// RedrivableFAILED returns FAILED rows whose reason is classified retryable or
// is an expected decline, or that have no job at all, oldest first, at most
// limit of them. A decline is recorded SKIPPED, so a failure carrying one was
// recorded by a handler that predates the decline, and running it again
// settles it. A failure is recorded together with its failed job, so one with
// no job predates the job queue, and its reason says nothing about the
// current handlers. The filter is in the query: the cap must apply to
// redrivable rows, not to all failures, or a workspace with many terminal
// failures hides its redrivable ones beyond the cap.
func (q *DoctorQueries) RedrivableFAILED(ctx context.Context, limit int) ([]DoctorDoc, error) {
	var reasons []string
	for _, r := range append(domain.RetryableReasons(), domain.DeclineReasons()...) {
		reasons = append(reasons, string(r))
	}
	rows, err := q.db.Pool.Query(ctx, `
		SELECT `+doctorDocColumns+`
		FROM documents
		WHERE processing_status = 'FAILED'
		  AND (metadata_headers->>'reason' = ANY($2::text[])
		       OR NOT EXISTS (SELECT 1 FROM ingestion_jobs j WHERE j.doc_id = documents.doc_id))
		ORDER BY updated_at
		LIMIT $1`, limit, reasons)
	if err != nil {
		return nil, fmt.Errorf("redrivable failed: %w", err)
	}
	defer rows.Close()
	return scanDoctorDocs(rows)
}

// doctorDocColumns are the columns scanDoctorDocs reads, in its order.
const doctorDocColumns = `doc_id::text, mime_type,
		       source_ref, raw_sha256, source_filename,
		       COALESCE(parent_doc_id::text, ''),
		       COALESCE(metadata_headers->>'reason', ''),
		       updated_at,
		       NOT EXISTS (SELECT 1 FROM ingestion_jobs j WHERE j.doc_id = documents.doc_id)`

// FAILEDWithReasons returns FAILED rows carrying any of reasons, whatever
// their class, or every FAILED row when reasons is empty, oldest first, at
// most limit of them: the selection an operator makes by naming reasons, or
// all of them.
func (q *DoctorQueries) FAILEDWithReasons(ctx context.Context, reasons []string, limit int) ([]DoctorDoc, error) {
	if reasons == nil {
		reasons = []string{}
	}
	rows, err := q.db.Pool.Query(ctx, `
		SELECT `+doctorDocColumns+`
		FROM documents
		WHERE processing_status = 'FAILED'
		  AND (cardinality($2::text[]) = 0 OR metadata_headers->>'reason' = ANY($2::text[]))
		ORDER BY updated_at
		LIMIT $1`, limit, reasons)
	if err != nil {
		return nil, fmt.Errorf("failed with reason: %w", err)
	}
	defer rows.Close()
	return scanDoctorDocs(rows)
}

// DoctorDoc is a lightweight document representation for doctor/recovery.
type DoctorDoc struct {
	DocID       string
	MimeType    string
	SourceRef   string
	RawSHA256   string
	SourceName  string
	ParentDocID string
	Reason      string
	UpdatedAt   time.Time
	// Jobless is a document with no job at all: one recorded before the
	// job queue existed.
	Jobless bool
}

func scanDoctorDocs(rows pgxRows) ([]DoctorDoc, error) {
	var out []DoctorDoc
	for rows.Next() {
		var d DoctorDoc
		if err := rows.Scan(&d.DocID, &d.MimeType,
			&d.SourceRef, &d.RawSHA256, &d.SourceName, &d.ParentDocID,
			&d.Reason, &d.UpdatedAt, &d.Jobless); err != nil {
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

// ErrJobLive refuses recovery of a document that has a job holding an
// unexpired lease: a handler may still be working on it.
var ErrJobLive = errors.New("a job for this document holds a live lease")

// ErrPlanChanged refuses an item whose jobs no longer match the plan, so a plan
// made before the state moved cannot reset work it never saw.
var ErrPlanChanged = errors.New("the document's jobs changed since the plan was made")

// DoctorJob is one job as the recovery planner sees it.
type DoctorJob struct {
	ID         string
	DocID      string
	State      string
	Generation int64
	Attempts   int
	// Live is true for a RUNNING job whose lease has not expired.
	Live bool
}

// JobsForDocuments returns the unfinished and failed jobs of the given
// documents. Completed jobs are omitted: recovery never touches them.
func (q *DoctorQueries) JobsForDocuments(ctx context.Context, docIDs []string) (map[string][]DoctorJob, error) {
	out := map[string][]DoctorJob{}
	if len(docIDs) == 0 {
		return out, nil
	}
	rows, err := q.db.Pool.Query(ctx, `SELECT job_id::text, doc_id::text, state, generation, attempts,
 (state = 'RUNNING' AND lease_until > clock_timestamp())
 FROM ingestion_jobs WHERE doc_id = ANY($1::uuid[]) AND state <> 'COMPLETED'
 ORDER BY created_at, job_id`, docIDs)
	if err != nil {
		return nil, fmt.Errorf("jobs for documents: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var j DoctorJob
		if err := rows.Scan(&j.ID, &j.DocID, &j.State, &j.Generation, &j.Attempts, &j.Live); err != nil {
			return nil, err
		}
		out[j.DocID] = append(out[j.DocID], j)
	}
	return out, rows.Err()
}

// JobStats is the queue state doctor reports.
type JobStats struct {
	// ByState counts jobs per state.
	ByState map[string]int
	// ExpiredLeases counts RUNNING jobs whose lease expired: their worker is
	// gone, and the next ingestion run reclaims them.
	ExpiredLeases int
	// ExhaustedBudgets counts unfinished jobs that spent every attempt and are
	// failed, not executed, on their next claim.
	ExhaustedBudgets int
	// FailedByReason groups FAILED jobs by closed reason.
	FailedByReason map[string]int
}

// JobStats reads the queue layout. Callers check QueueSchemaReady first: on an
// old layout these columns do not exist.
func (q *DoctorQueries) JobStats(ctx context.Context) (JobStats, error) {
	stats := JobStats{ByState: map[string]int{}, FailedByReason: map[string]int{}}
	rows, err := q.db.Pool.Query(ctx, `SELECT state, count(*),
 count(*) FILTER (WHERE state = 'RUNNING' AND lease_until <= clock_timestamp()),
 count(*) FILTER (WHERE attempts >= 3 AND (state = 'READY' OR (state = 'RUNNING' AND lease_until <= clock_timestamp())))
 FROM ingestion_jobs GROUP BY state`)
	if err != nil {
		return stats, fmt.Errorf("job stats: %w", err)
	}
	for rows.Next() {
		var state string
		var n, expired, exhausted int
		if err := rows.Scan(&state, &n, &expired, &exhausted); err != nil {
			rows.Close()
			return stats, err
		}
		stats.ByState[state] = n
		stats.ExpiredLeases += expired
		stats.ExhaustedBudgets += exhausted
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return stats, err
	}
	failed, err := q.db.Pool.Query(ctx, `SELECT COALESCE(NULLIF(reason, ''), 'UNKNOWN'), count(*)
 FROM ingestion_jobs WHERE state = 'FAILED' GROUP BY 1`)
	if err != nil {
		return stats, fmt.Errorf("failed jobs by reason: %w", err)
	}
	defer failed.Close()
	for failed.Next() {
		var reason string
		var n int
		if err := failed.Scan(&reason, &n); err != nil {
			return stats, err
		}
		stats.FailedByReason[reason] = n
	}
	return stats, failed.Err()
}

// ResetToPENDING returns a document's failed and expired jobs to the start,
// atomically, beginning a fresh retry cycle for those jobs only: attempts
// return to zero and the recorded failure is cleared. It is the only place
// recovery spends the retry budget, and it commits the document change, the
// job change and the wake-up notification together.
//
// Only eligible jobs are reset: FAILED jobs and RUNNING jobs whose lease has
// expired. READY jobs, delayed retries included, and WAITING_SOURCE jobs are
// left exactly as they are — resetting them would erase their recorded
// failures, refund their whole budget and discard their backoff, or retry a
// source wait before a complete scan has refreshed locations.
//
// expected is the plan's view of the document's jobs. The transaction
// revalidates it against durable state, refusing without mutation when a job
// holds a live lease or a handler for it is still running (ErrJobLive), or a
// job's state or generation moved (ErrPlanChanged). Each job's advisory lock is
// taken first and never waited for: an expired handler can still be physically
// running and holding both the lock and its document row, so recovery must
// neither block behind it nor deadlock with its finalization. The whole item
// runs under the control deadline. Returns true if the document row changed.
func (q *DoctorQueries) ResetToPENDING(ctx context.Context, docID string, expected []DoctorJob) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, ControlTimeout)
	defer cancel()
	changed := false
	err := q.db.Transaction(ctx, func(ctx context.Context) error {
		ex := q.db.executor(ctx)
		// Advisory exclusion first, in a fixed order.
		ids, err := q.openJobIDs(ctx, docID)
		if err != nil {
			return err
		}
		for _, id := range ids {
			var locked bool
			if err := ex.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))", id).Scan(&locked); err != nil {
				return err
			}
			if !locked {
				return ErrJobLive
			}
		}
		rows, err := ex.Query(ctx, `SELECT job_id::text, state, generation,
 (state = 'RUNNING' AND lease_until > clock_timestamp()),
 (state = 'FAILED' OR (state = 'RUNNING' AND lease_until <= clock_timestamp()))
 FROM ingestion_jobs WHERE doc_id = $1 AND state <> 'COMPLETED' ORDER BY job_id FOR UPDATE`, docID)
		if err != nil {
			return err
		}
		want := map[string]DoctorJob{}
		for _, j := range expected {
			want[j.ID] = j
		}
		var mismatch, live bool
		var eligible []string
		seen := 0
		for rows.Next() {
			var id, state string
			var generation int64
			var isLive, isEligible bool
			if err := rows.Scan(&id, &state, &generation, &isLive, &isEligible); err != nil {
				rows.Close()
				return err
			}
			seen++
			live = live || isLive
			if isEligible {
				eligible = append(eligible, id)
			}
			if w, ok := want[id]; !ok || w.State != state || w.Generation != generation {
				mismatch = true
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if live {
			return ErrJobLive
		}
		if mismatch || seen != len(want) {
			return ErrPlanChanged
		}
		// Nothing eligible but jobs remain: they are queued or waiting, and
		// they and their document stay as they are.
		if seen > 0 && len(eligible) == 0 {
			return nil
		}
		tag, err := ex.Exec(ctx, `UPDATE documents SET processing_status='PENDING',metadata_headers=metadata_headers-'reason',updated_at=now() WHERE doc_id=$1 AND processing_status IN ('PENDING','PROCESSING','FAILED')`, docID)
		if err != nil {
			return err
		}
		changed = tag.RowsAffected() > 0
		if !changed || len(eligible) == 0 {
			return nil
		}
		var n int
		return ex.QueryRow(ctx, `WITH u AS (
 UPDATE ingestion_jobs SET state='READY', generation=generation+1, attempts=0, reason='',
        last_failure_reason='', finished_at=NULL, lease_until=NULL, available_at=clock_timestamp()
 WHERE job_id = ANY($1::uuid[]) RETURNING 1)
 SELECT count(*) FROM (SELECT pg_notify($2,'') FROM (SELECT 1 FROM u LIMIT 1) one) n`, eligible, NotifyChannel).Scan(&n)
	})
	return changed, err
}

// openJobIDs lists a document's unfinished and failed jobs in lock order.
func (q *DoctorQueries) openJobIDs(ctx context.Context, docID string) ([]string, error) {
	rows, err := q.db.executor(ctx).Query(ctx, `SELECT job_id::text FROM ingestion_jobs WHERE doc_id = $1 AND state <> 'COMPLETED' ORDER BY job_id`, docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
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
