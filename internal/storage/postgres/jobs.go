package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	ingestionv1 "github.com/suankan/pocket-advisor/api/proto/v1/gen"
	"github.com/suankan/pocket-advisor/internal/domain"
	"google.golang.org/protobuf/proto"
)

var ErrWorkspaceBusy = errors.New("another ingestion or maintenance process owns this workspace")
var ErrJobBusy = errors.New("job is already running")
var ErrInvalidJob = errors.New("invalid or incompatible ingestion job")

// ErrLeaseLost reports that this claim no longer owns its job: the lease
// expired, a successor claimed it, or the row is gone. The caller records no
// verdict; expiry or the successor resolves the job.
var ErrLeaseLost = errors.New("job ownership lost")

// ErrCommitUncertain wraps a failed COMMIT, whose outcome the caller must read
// from durable state with State before it counts the job as done or failed.
var ErrCommitUncertain = errors.New("job commit outcome uncertain")

// Subjects are the stage queues. Every subject a producer can emit must have a
// consuming pool, or work accumulates silently. The subject is stored on each
// job row, so these values name durable data and must not change.
const (
	SubjectEmails = "ingest.emails.raw"
	SubjectPDFs   = "ingest.pdfs.raw"
	SubjectDocx   = "ingest.docx.raw"
	SubjectImages = "ingest.images.raw"
	SubjectEmbed  = "ingest.text.embed"
)

// Queue constants. They are settled design values, not operator knobs.
const (
	// MaxAttempts is the application's failure threshold: a charged claim is
	// one attempt, whether it ended in an error or in a crash. The third
	// transient error fails the job.
	MaxAttempts = 3
	// LeaseDuration is how long a claim stays valid without renewal.
	LeaseDuration = 5 * time.Minute
	// HandlerBudget caps a claim's total lifetime and the handler's deadline.
	// It is sized off the slowest legitimate document (a 208-page scanned PDF
	// takes ~15 minutes of OCR), leaving fourfold headroom while keeping a
	// wedged handler from holding its job for the life of the process.
	HandlerBudget = time.Hour
	// ControlTimeout bounds every claim, renewal and verdict statement,
	// including connection acquisition.
	ControlTimeout = 5 * time.Second
	// BusyDelay spaces out a claim that found the job's advisory lock held by
	// an expired handler that is still physically running.
	BusyDelay = 30 * time.Second
	// DeadlockDelay spaces out a job whose transaction PostgreSQL aborted to
	// break a deadlock: long enough for the transaction it lost to to commit,
	// short enough that a lost race does not read as a backlog.
	DeadlockDelay = 5 * time.Second

	// NotifyChannel is the one fixed LISTEN/NOTIFY channel per database. The
	// payload is always empty: a notification is only a hint to look.
	NotifyChannel = "pocket_advisor_jobs"
)

// Job is one claimed unit of work. Generation identifies the claim; every
// verdict for it must present the generation and an unexpired lease.
type Job struct {
	ID, DocID, Subject, Traceparent string
	Payload                         []byte
	Generation                      int64
	// Attempts counts charged claims in the current retry cycle, including
	// this one when Charged.
	Attempts int
	// Charged is false when the retry budget was already exhausted at claim
	// time. Such a claim authorizes failure bookkeeping only.
	Charged bool
	// LastFailure is the closed reason of the latest processing failure.
	LastFailure domain.FailureReason

	// ClaimedAt, LeaseUntil and ServerNow are database time.
	ClaimedAt, LeaseUntil, ServerNow time.Time
	// Started is the local monotonic instant before the claim request was
	// sent, so deadlines derived from it include connection-acquisition delay.
	Started time.Time
}

// Lease is the answer to a successful renewal, in database time.
type Lease struct{ Until, ServerNow time.Time }

// JobState is the durable state of one job, read to resolve ambiguity.
type JobState struct {
	Exists     bool
	State      string
	Generation int64
}

type Jobs struct {
	db    *DB
	gate  *Gate
	owner string
	// flips holds one alternation counter per subject: pools claim concurrently,
	// and a shared counter could hand one subject the same parity every time.
	flips sync.Map
}

// flip advances and returns a subject's alternation counter.
func (r *Jobs) flip(subject string) uint32 {
	c, _ := r.flips.LoadOrStore(subject, new(atomic.Uint32))
	return c.(*atomic.Uint32).Add(1)
}

func NewJobs(db *DB) *Jobs {
	return &Jobs{
		db:    db,
		gate:  NewGate(max(1, int(db.Pool.Config().MaxConns)-3)),
		owner: uuid.NewString(),
	}
}

// Gate is the process-wide bound on concurrently open stage transactions.
func (r *Jobs) Gate() *Gate { return r.gate }

// Publish enqueues a command in the caller's transaction, so the job commits
// together with the rows that make it resolvable, and wakes listening pools at
// that same commit.
func (r *Jobs) Publish(ctx context.Context, subject string, msg proto.Message, traceparent string) error {
	m, ok := msg.(interface {
		GetMetadata() *ingestionv1.DocumentMetadata
	})
	if !ok || m.GetMetadata() == nil {
		return ErrInvalidJob
	}
	meta := m.GetMetadata()
	payload, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	// Stable stage identity is raw bytes plus canonical extraction for embedding.
	revision := meta.RawSha256
	if _, ok := msg.(*ingestionv1.EmbedTextCommand); ok {
		var hash string
		if err := r.db.executor(ctx).QueryRow(ctx, "SELECT extraction_hash FROM documents WHERE doc_id=$1", meta.DocId).Scan(&hash); err != nil {
			return err
		}
		revision += "/" + hash
	}
	_, err = r.db.executor(ctx).Exec(ctx, `WITH ins AS (
 INSERT INTO ingestion_jobs(doc_id,subject,revision,payload,traceparent)
 VALUES($1,$2,$3,$4,$5) ON CONFLICT(doc_id,subject,revision) DO NOTHING RETURNING 1)
 SELECT pg_notify($6,'') FROM ins`, meta.DocId, subject, revision, payload, traceparent, NotifyChannel)
	return err
}

// claimStatement fixes the claim semantics. The selection is a single
// statement so a claim is atomic and committed before any handler runs. The
// wall time is materialized once so it is a stable index cutoff rather than a
// volatile call that would turn the cutoff into a row filter.
const claimStatement = `WITH claim_clock AS MATERIALIZED (
  SELECT clock_timestamp() AS claimed_at
), selected AS (
  SELECT job_id, attempts
  FROM ingestion_jobs
  WHERE subject = $1 AND %s
  ORDER BY %s, created_at, job_id
  LIMIT $2
  FOR UPDATE SKIP LOCKED
)
UPDATE ingestion_jobs j
SET state = 'RUNNING',
    attempts = LEAST(j.attempts + 1, 3),
    generation = j.generation + 1,
    claimed_at = (SELECT claimed_at FROM claim_clock),
    claimed_by = $3,
    lease_until = (SELECT claimed_at FROM claim_clock) + make_interval(secs => $4::double precision)
FROM selected s
WHERE j.job_id = s.job_id
RETURNING j.job_id::text, j.doc_id::text, j.subject, j.payload, j.traceparent,
          j.generation, j.attempts, j.claimed_at, j.lease_until,
          clock_timestamp(), (s.attempts < 3), j.last_failure_reason`

var (
	claimReadySQL = fmt.Sprintf(claimStatement,
		"state = 'READY' AND available_at <= (SELECT claimed_at FROM claim_clock)", "available_at")
	claimExpiredSQL = fmt.Sprintf(claimStatement,
		"state = 'RUNNING' AND lease_until <= (SELECT claimed_at FROM claim_clock)", "lease_until")
)

// Claim moves up to limit jobs for subject to RUNNING under a new generation
// and lease. READY work whose delay has elapsed and RUNNING work whose lease
// expired are separate statements, each following its own partial index.
//
// Neither state may starve the other. With several reservations one expired
// claim is tried before the remaining slots are filled from READY work; with
// one reservation the two states alternate as the first choice, and the other
// is tried when the first is empty.
func (r *Jobs) Claim(ctx context.Context, subject string, limit int) ([]Job, error) {
	if limit < 1 {
		return nil, nil
	}
	if limit > 1 {
		jobs, err := r.claim(ctx, claimExpiredSQL, subject, 1)
		if err != nil {
			return nil, err
		}
		more, err := r.claim(ctx, claimReadySQL, subject, limit-len(jobs))
		return append(jobs, more...), err
	}
	order := []string{claimReadySQL, claimExpiredSQL}
	if r.flip(subject)%2 == 0 {
		order[0], order[1] = order[1], order[0]
	}
	for _, statement := range order {
		jobs, err := r.claim(ctx, statement, subject, 1)
		if err != nil || len(jobs) > 0 {
			return jobs, err
		}
	}
	return nil, nil
}

func (r *Jobs) claim(ctx context.Context, statement, subject string, limit int) ([]Job, error) {
	ctx, cancel := context.WithTimeout(ctx, ControlTimeout)
	defer cancel()
	started := time.Now()
	rows, err := r.db.Pool.Query(ctx, statement, subject, limit, r.owner, LeaseDuration.Seconds())
	if err != nil {
		return nil, err
	}
	// Every row is consumed and the result closed before any handoff, so the
	// statement has fully completed and its connection is back in the pool.
	defer rows.Close()
	var jobs []Job
	for rows.Next() {
		j := Job{Started: started}
		var failure string
		if err := rows.Scan(&j.ID, &j.DocID, &j.Subject, &j.Payload, &j.Traceparent,
			&j.Generation, &j.Attempts, &j.ClaimedAt, &j.LeaseUntil, &j.ServerNow, &j.Charged, &failure); err != nil {
			return nil, err
		}
		j.LastFailure = domain.FailureReason(failure)
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// Renew extends a claim's lease. Zero rows is ErrLeaseLost: the lease expired,
// the handler budget ended, or another generation owns the job. Any other error
// leaves ownership uncertain.
func (r *Jobs) Renew(ctx context.Context, j Job) (Lease, error) {
	var l Lease
	err := r.db.Pool.QueryRow(ctx, `UPDATE ingestion_jobs
SET lease_until = LEAST(clock_timestamp() + make_interval(secs => $3::double precision),
                        claimed_at + make_interval(secs => $4::double precision))
WHERE job_id = $1 AND generation = $2 AND state = 'RUNNING'
  AND lease_until > clock_timestamp()
  AND claimed_at + make_interval(secs => $4::double precision) > clock_timestamp()
RETURNING lease_until, clock_timestamp()`,
		j.ID, j.Generation, LeaseDuration.Seconds(), HandlerBudget.Seconds()).Scan(&l.Until, &l.ServerNow)
	if errors.Is(err, pgx.ErrNoRows) {
		return l, ErrLeaseLost
	}
	return l, err
}

// Run executes fn inside one READ COMMITTED transaction that commits every
// stage write, downstream job and the job's own completion together.
//
// The transaction holds the job's advisory lock for mutual exclusion with an
// expired handler that is still physically running, and takes no job-row lock
// while fn runs, so renewals from another connection proceed. The final
// completion statement is the ownership fence: zero rows rolls back
// everything and returns ErrLeaseLost.
func (r *Jobs) Run(ctx context.Context, j Job, fn func(context.Context) error) error {
	if parsed, err := uuid.Parse(j.ID); err != nil || parsed.String() != j.ID {
		return ErrInvalidJob
	}
	tx, err := r.db.executor(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	ctx = context.WithValue(ctx, transactionKey{r.db}, tx)

	var locked bool
	if err := tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))", j.ID).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return ErrJobBusy
	}
	var state string
	var generation int64
	var live bool
	err = tx.QueryRow(ctx, `SELECT state, generation,
 COALESCE(lease_until > clock_timestamp() AND claimed_at + make_interval(secs => $2::double precision) > clock_timestamp(), false)
 FROM ingestion_jobs WHERE job_id = $1`, j.ID, HandlerBudget.Seconds()).Scan(&state, &generation, &live)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLeaseLost
	}
	if err != nil {
		return err
	}
	if state != "RUNNING" || generation != j.Generation || !live {
		return ErrLeaseLost
	}
	if err := fn(ctx); err != nil {
		return err
	}
	// Finalization runs under its own bound and is not cancelled with the
	// handler: a handler that finished while the run was being torn down still
	// has its ownership decided by the fence, not by which context expired
	// first.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ControlTimeout)
	defer cancel()
	tag, err := tx.Exec(fctx, `UPDATE ingestion_jobs
SET state = 'COMPLETED', finished_at = clock_timestamp(),
    lease_until = NULL, reason = '', last_failure_reason = ''
WHERE job_id = $1 AND generation = $2 AND state = 'RUNNING'
  AND lease_until > clock_timestamp()
  AND claimed_at + make_interval(secs => $3::double precision) > clock_timestamp()`,
		j.ID, j.Generation, HandlerBudget.Seconds())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	if err := tx.Commit(fctx); err != nil {
		return fmt.Errorf("%w: %v", ErrCommitUncertain, err)
	}
	return nil
}

// State reads a job's durable state, to resolve an uncertain commit or verdict.
func (r *Jobs) State(ctx context.Context, j Job) (JobState, error) {
	var s JobState
	err := r.db.Pool.QueryRow(ctx, "SELECT state, generation FROM ingestion_jobs WHERE job_id = $1", j.ID).Scan(&s.State, &s.Generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, nil
	}
	s.Exists = err == nil
	return s, err
}

// owned is the predicate every verdict carries: this exact claim, still
// running, with an unexpired lease. A stale worker therefore cannot overwrite a
// successor or recreate a deleted job.
const owned = `job_id = $1 AND generation = $2 AND state = 'RUNNING' AND lease_until > clock_timestamp()`

// transition runs one guarded UPDATE and reports ErrLeaseLost when the guard
// matched nothing. With notify, pools are woken at the commit that makes the
// work READY.
func (r *Jobs) transition(ctx context.Context, notify bool, update string, args ...any) error {
	ctx, cancel := context.WithTimeout(ctx, ControlTimeout)
	defer cancel()
	statement := "WITH u AS (" + update + " RETURNING 1) SELECT count(*) FROM u"
	if notify {
		statement = "WITH u AS (" + update + " RETURNING 1) SELECT count(*) FROM (SELECT pg_notify('" + NotifyChannel + "','') FROM u) n"
	}
	var n int
	if err := r.db.Pool.QueryRow(ctx, statement, args...).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrLeaseLost
	}
	return nil
}

func refund(j Job) int {
	if j.Charged {
		return 1
	}
	return 0
}

// Retry returns an owned job to READY after delay. The charged attempt is
// kept and the closed processing reason recorded.
func (r *Jobs) Retry(ctx context.Context, j Job, delay time.Duration, reason domain.FailureReason) error {
	return r.transition(ctx, true, `UPDATE ingestion_jobs
SET state = 'READY', available_at = clock_timestamp() + make_interval(secs => $3::double precision),
    lease_until = NULL, last_failure_reason = $4
WHERE `+owned, j.ID, j.Generation, delay.Seconds(), string(reason))
}

// Release returns an owned job to READY without a processing verdict, after
// delay: shutdown, cancellation before handoff, or a busy advisory lock. The
// attempt this claim charged is refunded, once, because the fence leaves
// RUNNING.
func (r *Jobs) Release(ctx context.Context, j Job, delay time.Duration) error {
	return r.transition(ctx, true, `UPDATE ingestion_jobs
SET state = 'READY', available_at = clock_timestamp() + make_interval(secs => $3::double precision),
    lease_until = NULL, attempts = GREATEST(attempts - $4, 0)
WHERE `+owned, j.ID, j.Generation, delay.Seconds(), refund(j))
}

// Wait parks an owned job until its original is available again. The charged
// attempt is refunded and the previous processing failure is left untouched,
// so source diagnostics never become a poison-work classification.
func (r *Jobs) Wait(ctx context.Context, j Job, reason string) error {
	return r.transition(ctx, false, `UPDATE ingestion_jobs
SET state = 'WAITING_SOURCE', reason = $3, lease_until = NULL, attempts = GREATEST(attempts - $4, 0)
WHERE `+owned, j.ID, j.Generation, reason, refund(j))
}

// Fail marks an owned job and its document FAILED in one transaction. The
// job's advisory lock is taken first and never waited for: an expired handler
// of this job can still be physically running, holding that lock and the
// document row, and its effects must not meet a failure verdict. A held lock is
// ErrJobBusy, the same answer a duplicate run gets. The document is then
// updated before the job fence, so a verdict never waits on a document lock
// while holding the job-row lock a stale handler's finalization needs. Zero
// rows rolls the document change back.
func (r *Jobs) Fail(ctx context.Context, j Job, reason domain.FailureReason) error {
	ctx, cancel := context.WithTimeout(ctx, ControlTimeout)
	defer cancel()
	return r.db.Transaction(ctx, func(ctx context.Context) error {
		var locked bool
		if err := r.db.executor(ctx).QueryRow(ctx, "SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))", j.ID).Scan(&locked); err != nil {
			return err
		}
		if !locked {
			return ErrJobBusy
		}
		if err := NewDocumentRepo(r.db).UpdateStatus(ctx, j.DocID, domain.StatusFailed, reason); err != nil {
			return err
		}
		tag, err := r.db.executor(ctx).Exec(ctx, `UPDATE ingestion_jobs
SET state = 'FAILED', reason = $3, last_failure_reason = $3, finished_at = clock_timestamp(), lease_until = NULL
WHERE `+owned, j.ID, j.Generation, string(reason))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrLeaseLost
		}
		return nil
	})
}

// ResumeSources retries source waits after a filesystem scan has refreshed
// locations. Charged attempts are retained.
func (r *Jobs) ResumeSources(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, ControlTimeout)
	defer cancel()
	var n int
	return r.db.Pool.QueryRow(ctx, `WITH u AS (
 UPDATE ingestion_jobs SET state = 'READY', generation = generation + 1,
        available_at = clock_timestamp(), lease_until = NULL
 WHERE state = 'WAITING_SOURCE' RETURNING 1)
 SELECT count(*) FROM (SELECT pg_notify($1,'') FROM (SELECT 1 FROM u LIMIT 1) one) n`, NotifyChannel).Scan(&n)
}

// Counts reports jobs by the groups drain and resolution care about. ready is
// READY work including delayed retries; unfinished adds RUNNING work.
func (r *Jobs) Counts(ctx context.Context) (ready, unfinished, waiting, failed int, err error) {
	ctx, cancel := context.WithTimeout(ctx, ControlTimeout)
	defer cancel()
	err = r.db.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state = 'READY'),
 count(*) FILTER (WHERE state IN ('READY','RUNNING')),
 count(*) FILTER (WHERE state = 'WAITING_SOURCE'), count(*) FILTER (WHERE state = 'FAILED')
 FROM ingestion_jobs`).Scan(&ready, &unfinished, &waiting, &failed)
	return
}

// Clock reads the database clock, the one job verdicts are stamped with, so a
// run can mark its start without trusting the host's clock to agree.
func (r *Jobs) Clock(ctx context.Context) (time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, ControlTimeout)
	defer cancel()
	var now time.Time
	err := r.db.Pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now)
	return now, err
}

// CheckResolved reports what a finite run that started at since leaves
// unresolved: unfinished work, source waits, and jobs that failed during the
// run. Jobs that had already failed before since are returned as a count
// rather than an error. Nothing this run did could have resolved them, since
// ingestion never retries a failure on its own, so counting them would end
// every later run unresolved until each was fixed or removed; --recover is
// what retries them.
func (r *Jobs) CheckResolved(ctx context.Context, since time.Time) (failedBefore int, err error) {
	ctx, cancel := context.WithTimeout(ctx, ControlTimeout)
	defer cancel()
	var u, w, f int
	if err := r.db.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state IN ('READY','RUNNING')),
 count(*) FILTER (WHERE state = 'WAITING_SOURCE'),
 count(*) FILTER (WHERE state = 'FAILED' AND finished_at >= $1),
 count(*) FILTER (WHERE state = 'FAILED' AND (finished_at IS NULL OR finished_at < $1))
 FROM ingestion_jobs`, since).Scan(&u, &w, &f, &failedBefore); err != nil {
		return 0, err
	}
	if u+w+f > 0 {
		err := fmt.Errorf("ingestion unresolved: %d unfinished, %d waiting for sources, %d jobs failed in this run", u, w, f)
		if w > 0 {
			// Ingestion never deletes, so work whose original was removed
			// stays waiting until the operator prunes or restores it.
			err = fmt.Errorf("%w; --reconcile-deletions removes documents whose originals are gone, "+
				"--reconcile retries restored originals", err)
		}
		return failedBefore, err
	}
	return failedBefore, nil
}

// Depth is one subject's outstanding work and when it next becomes due, in
// database time.
type Depth struct {
	// Pending is READY plus RUNNING, delayed and expired work included.
	Pending int
	// NextReady is the earliest availability among READY jobs; NextExpiry is
	// the earliest lease expiry among RUNNING jobs. Zero when there are none.
	NextReady, NextExpiry time.Time
	// ServerNow is the database clock when the sample was taken.
	ServerNow time.Time
}

// Depths samples every subject with one grouped query.
func (r *Jobs) Depths(ctx context.Context) (map[string]Depth, error) {
	ctx, cancel := context.WithTimeout(ctx, ControlTimeout)
	defer cancel()
	rows, err := r.db.Pool.Query(ctx, `SELECT subject, count(*),
 min(available_at) FILTER (WHERE state = 'READY'),
 min(lease_until) FILTER (WHERE state = 'RUNNING'),
 clock_timestamp()
 FROM ingestion_jobs WHERE state IN ('READY','RUNNING') GROUP BY subject`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Depth{}
	for rows.Next() {
		var subject string
		var d Depth
		var ready, expiry *time.Time
		if err := rows.Scan(&subject, &d.Pending, &ready, &expiry, &d.ServerNow); err != nil {
			return nil, err
		}
		if ready != nil {
			d.NextReady = *ready
		}
		if expiry != nil {
			d.NextExpiry = *expiry
		}
		out[subject] = d
	}
	return out, rows.Err()
}
