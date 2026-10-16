package postgres

import (
	"context"
	"slices"
	"time"

	"github.com/suankan/pocket-advisor/internal/domain"
)

// MaxAttention bounds how many documents a run summary names. The counts
// beside the list are always complete.
const MaxAttention = 50

// Tally is one reason and how many documents carry it.
type Tally struct {
	Reason string
	Count  int
}

// AttentionDoc is a document only the owner can resolve: a failure, or content
// locked behind a password. Location is the original's path, or the member's
// name and the path of a container that holds it.
type AttentionDoc struct {
	Status   domain.Status
	Reason   string
	ThisRun  bool
	Location string
}

// RunSummary is what one ingestion run did and what remains undone, read from
// the store rather than counted in memory, so it is also right for a run that
// was interrupted or failed partway.
type RunSummary struct {
	// Files is the originals the run's scans observed; NewOriginals those
	// among them whose content was admitted for the first time.
	Files, NewOriginals int
	// Settled is the documents this run created or finished a job for, by
	// their current status and then reason. PENDING and PROCESSING mean the run
	// ended before they did.
	Settled map[domain.Status][]Tally
	// Unfinished and Waiting are jobs still READY or RUNNING, and jobs waiting
	// for their original.
	Unfinished, Waiting int
	// FailedThisRun and FailedEarlier split the failed documents by whether
	// their failure was recorded during this run.
	FailedThisRun, FailedEarlier []Tally
	// Locked counts documents skipped for a password; LockedThisRun those this
	// run settled.
	Locked, LockedThisRun int
	// Unsupported groups every document skipped as an unsupported format by
	// type, across the store rather than this run, naming one of each: what
	// the corpus holds that no handler reads yet.
	Unsupported []TypeGroup
	// Attention names the failed documents and this run's password skips, at
	// most MaxAttention of them, this run's first; AttentionTotal is how many
	// there are in all.
	Attention      []AttentionDoc
	AttentionTotal int
}

// RunSummary reads the summary of a run that started at since, by the
// database clock (Clock).
func (r *Jobs) RunSummary(ctx context.Context, since time.Time) (RunSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, ControlTimeout)
	defer cancel()
	var s RunSummary
	locked := make([]string, 0, 2)
	for _, reason := range domain.PasswordReasons() {
		locked = append(locked, string(reason))
	}
	q := r.db.Pool

	if err := q.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM source_occurrences o JOIN source_scans s USING (scan_id) WHERE s.started_at >= $1),
 (SELECT count(*) FROM documents WHERE parent_doc_id IS NULL AND created_at >= $1),
 (SELECT count(*) FROM ingestion_jobs WHERE state IN ('READY','RUNNING')),
 (SELECT count(*) FROM ingestion_jobs WHERE state = 'WAITING_SOURCE'),
 (SELECT count(*) FROM documents WHERE processing_status = 'SKIPPED' AND metadata_headers->>'reason' = ANY($2))`,
		since, locked).Scan(&s.Files, &s.NewOriginals, &s.Unfinished, &s.Waiting, &s.Locked); err != nil {
		return s, err
	}

	// A document belongs to this run when the run created it or finished one
	// of its jobs. Discovery settles some documents without any job, such as
	// unsupported formats, so jobs alone would miss them.
	rows, err := q.Query(ctx, `WITH touched AS (
   SELECT doc_id FROM ingestion_jobs WHERE finished_at >= $1
   UNION SELECT doc_id FROM documents WHERE created_at >= $1)
 SELECT d.processing_status::text, COALESCE(d.metadata_headers->>'reason', ''), count(*)
 FROM documents d JOIN touched USING (doc_id)
 GROUP BY 1, 2 ORDER BY 1, 3 DESC, 2`, since)
	if err != nil {
		return s, err
	}
	s.Settled = map[domain.Status][]Tally{}
	for rows.Next() {
		var status string
		var t Tally
		if err := rows.Scan(&status, &t.Reason, &t.Count); err != nil {
			rows.Close()
			return s, err
		}
		s.Settled[domain.Status(status)] = append(s.Settled[domain.Status(status)], t)
		if domain.Status(status) == domain.StatusSkipped && slices.Contains(locked, t.Reason) {
			s.LockedThisRun += t.Count
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return s, err
	}

	// A failed document's failure belongs to this run when its latest failed
	// job finished during it.
	rows, err = q.Query(ctx, `SELECT f.this_run, COALESCE(d.metadata_headers->>'reason', ''), count(*)
 FROM (SELECT DISTINCT ON (doc_id) doc_id, COALESCE(finished_at >= $1, false) AS this_run
       FROM ingestion_jobs WHERE state = 'FAILED'
       ORDER BY doc_id, finished_at DESC NULLS LAST) f
 JOIN documents d USING (doc_id)
 GROUP BY 1, 2 ORDER BY 1, 3 DESC, 2`, since)
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var thisRun bool
		var t Tally
		if err := rows.Scan(&thisRun, &t.Reason, &t.Count); err != nil {
			rows.Close()
			return s, err
		}
		if thisRun {
			s.FailedThisRun = append(s.FailedThisRun, t)
		} else {
			s.FailedEarlier = append(s.FailedEarlier, t)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return s, err
	}

	if s.Unsupported, err = unsupportedByType(ctx, r.db, 1); err != nil {
		return s, err
	}

	rows, err = q.Query(ctx, `WITH picked AS (
   SELECT DISTINCT ON (doc_id) doc_id, COALESCE(finished_at >= $1, false) AS this_run
   FROM ingestion_jobs WHERE state = 'FAILED'
   ORDER BY doc_id, finished_at DESC NULLS LAST)
 , locked AS (
   SELECT d.doc_id, true AS this_run FROM documents d
   WHERE d.processing_status = 'SKIPPED' AND d.metadata_headers->>'reason' = ANY($2)
     AND (d.created_at >= $1 OR EXISTS (SELECT 1 FROM ingestion_jobs j WHERE j.doc_id = d.doc_id AND j.finished_at >= $1)))
 , listed AS (SELECT * FROM picked UNION ALL SELECT * FROM locked)
 SELECT d.processing_status::text, COALESCE(d.metadata_headers->>'reason', ''), l.this_run,
        `+locationColumns+`,
        count(*) OVER ()
 FROM listed l JOIN documents d USING (doc_id)
 `+locationJoin+`
 ORDER BY l.this_run DESC, 1, 2, 5, 6, 4
 LIMIT $3`, since, locked, MaxAttention)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var a AttentionDoc
		var status, name, own, containerPath, containerName string
		if err := rows.Scan(&status, &a.Reason, &a.ThisRun, &name, &own, &containerPath, &containerName, &s.AttentionTotal); err != nil {
			return s, err
		}
		a.Status = domain.Status(status)
		a.Location = location(name, own, containerPath, containerName)
		s.Attention = append(s.Attention, a)
	}
	return s, rows.Err()
}
