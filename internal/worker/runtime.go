// Package worker is the transport layer: it claims jobs from the PostgreSQL
// queue, calls an engine, and settles each job with a fenced verdict
// (ingestion-design.md §8.2).
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/source"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/telemetry"
)

// Terminal marks an error as not worth retrying: the work is broken in a way
// a retry cannot fix, so the job goes straight to FAILED.
type Terminal struct {
	Reason domain.FailureReason
	Err    error
}

func (t *Terminal) Error() string { return string(t.Reason) + ": " + t.Err.Error() }
func (t *Terminal) Unwrap() error { return t.Err }

func Fatal(reason domain.FailureReason, err error) error {
	return &Terminal{Reason: reason, Err: err}
}

// Declined marks work the system knowingly does not support. It is NOT a
// failure: the document is recorded SKIPPED and the job completes. Mixing
// "we can't parse this" with "this broke" makes failed work unactionable
// (§2.5).
type Declined struct {
	Reason domain.FailureReason
	DocID  string
}

func (d *Declined) Error() string { return "declined: " + string(d.Reason) }

func Decline(docID string, reason domain.FailureReason) error {
	return &Declined{Reason: reason, DocID: docID}
}

// Handler processes one claimed job. Returning nil completes it.
type Handler func(ctx context.Context, job postgres.Job) error

// Queue is the half of the job repository the runtime needs, narrowed to an
// interface so every verdict path is testable without a database.
// *postgres.Jobs is the only production implementation.
type Queue interface {
	Claim(ctx context.Context, subject string, limit int) ([]postgres.Job, error)
	Renew(ctx context.Context, j postgres.Job) (postgres.Lease, error)
	Run(ctx context.Context, j postgres.Job, fn func(context.Context) error) error
	State(ctx context.Context, j postgres.Job) (postgres.JobState, error)
	Retry(ctx context.Context, j postgres.Job, delay time.Duration, reason domain.FailureReason) error
	Release(ctx context.Context, j postgres.Job, delay time.Duration) error
	Wait(ctx context.Context, j postgres.Job, reason string) error
	Fail(ctx context.Context, j postgres.Job, reason domain.FailureReason) error
}

// DocumentStore is the half of Tier 2 the runtime itself touches: recording a
// decline against a document, and asking whether one is still there. Narrowed
// for the same reason Queue is — these are failure paths, and a failure
// path that can only be exercised against a live database is a failure path
// that goes untested. *postgres.DocumentRepo is the only production
// implementation.
type DocumentStore interface {
	UpdateStatus(ctx context.Context, docID string, s domain.Status, reason domain.FailureReason) error
	Exists(ctx context.Context, docID string) (bool, error)
}

// Queue timings are settled design constants. They are variables only so the
// package's own tests can shorten them; nothing configures them at runtime.
var (
	// renewEvery is how often a running handler renews its lease.
	renewEvery = 30 * time.Second
	// renewMargin is the safety window: a handler whose lease has not been
	// confirmed this long before its conservative local expiry is cancelled.
	renewMargin = 30 * time.Second
	// renewRetryMin and renewRetryMax bound the jittered delay between
	// renewal retries after a transient error.
	renewRetryMin = time.Second
	renewRetryMax = 5 * time.Second
	// pollFallback is the longest an idle pool waits without a notification
	// or a known due time.
	pollFallback = 5 * time.Second
	// claimErrorWait spaces out claim retries while the database is failing.
	claimErrorWait = time.Second
)

// nakBackoffBase exceeds the embedding breaker's 30s cooldown, so a retry
// that waits at least half of it still finds the breaker willing to try
// rather than certain to refuse. nakBackoffMax keeps the wait bounded for
// the slower failures, where retrying promptly costs more than it saves.
const (
	nakBackoffBase = 60 * time.Second
	nakBackoffMax  = 5 * time.Minute
)

// errLeaseUncertain cancels a handler whose lease could not be confirmed
// inside the safety window. Ownership is left to expire with its attempt
// still charged.
var errLeaseUncertain = errors.New("lease renewal unconfirmed inside the safety window")

type Runtime struct {
	Queue   Queue
	Gate    *postgres.Gate
	Name    string
	Docs    DocumentStore
	Log     *slog.Logger
	Subject string

	// Lanes is how many jobs this pool handles at once.
	Lanes int

	// Stats tracks this subject's active lanes for drain detection. Optional:
	// nil leaves the subject out of it.
	Stats *telemetry.Queue

	setup sync.Once
	wake  chan struct{}
	// due is the earliest known time (local clock, unix nanos) this subject
	// has work becoming claimable; consumed is the last hint already acted on,
	// so an unchanged stale hint cannot schedule immediate claims forever.
	due, consumed atomic.Int64
}

func (r *Runtime) init() { r.setup.Do(func() { r.wake = make(chan struct{}, 1) }) }

// Wake nudges the pool to look for work. It is a signal, not a queue: a wake
// that arrives while one is pending is absorbed.
func (r *Runtime) Wake() {
	r.init()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// SetDue records when this subject next has work becoming claimable, from a
// depth sample converted to the local clock. A hint that moves the pool's next
// look sooner wakes it; an unchanged or already-consumed one is ignored.
func (r *Runtime) SetDue(t time.Time) {
	if t.IsZero() {
		return
	}
	n := t.UnixNano()
	if n == r.consumed.Load() {
		return
	}
	prev := r.due.Swap(n)
	if prev == 0 || n < prev {
		if t.After(time.Now()) {
			r.Wake()
		}
	}
}

// Consume runs a bounded worker pool until fetchCtx is done, then drains.
//
// The two contexts have deliberately different lifetimes. fetchCtx ending stops
// new work being claimed; workCtx bounds the handlers themselves and must
// outlive it. That gap is what makes Ctrl+C safe: in-flight documents finish
// and complete normally instead of being abandoned mid-flight.
func (r *Runtime) Consume(fetchCtx, workCtx context.Context, h Handler) error {
	r.init()
	lanes := max(r.Lanes, 1)

	work := make(chan postgres.Job)
	freed := make(chan struct{}, 1)
	var inFlight atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < lanes; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range work {
				r.handle(workCtx, job, h)
				inFlight.Add(-1)
				// The slot is held through the handler and its bookkeeping.
				r.Gate.Release(1)
				// A buffered-one channel and a non-blocking send make this a
				// signal rather than a queue.
				select {
				case freed <- struct{}{}:
				default:
				}
			}
		}()
	}

	r.claimLoop(fetchCtx, workCtx, work, freed, &inFlight, lanes)

	// Closing work lets the lanes finish what they hold and exit. Nothing new
	// enters after this point.
	close(work)
	wg.Wait()
	return nil
}

// claimLoop claims only as many jobs as there are idle lanes and free gate
// slots.
//
// Claiming more would park jobs in an in-process buffer where their lease
// keeps running while they wait. Capacity is reserved before the claim and
// whatever the claim does not use is released at once, so no pool ever holds a
// partial batch while waiting for the rest.
func (r *Runtime) claimLoop(
	ctx, workCtx context.Context,
	work chan<- postgres.Job,
	freed <-chan struct{},
	inFlight *atomic.Int64,
	lanes int,
) {
	for ctx.Err() == nil {
		free := lanes - int(inFlight.Load())
		if free < 1 {
			r.park(ctx, freed, pollFallback)
			continue
		}

		// Take the release channel before reserving, so a release between the
		// two cannot be missed.
		released := r.Gate.Wait()
		n := r.Gate.TryReserve(free)
		if n == 0 {
			r.park(ctx, released, pollFallback)
			continue
		}

		// The claim runs to completion even if claiming is being stopped: it is
		// a short, bounded statement, and cancelling it client-side could leave
		// committed claims that nothing owns until their leases expire. What it
		// returns is handed off, or handed back below.
		jobs, err := r.Queue.Claim(context.WithoutCancel(ctx), r.Subject, n)
		if surplus := n - len(jobs); surplus > 0 {
			r.Gate.Release(surplus)
		}
		if err != nil {
			if ctx.Err() == nil {
				r.Log.Warn("claim failed", "worker_type", r.Name, "subject", r.Subject, "error", err)
			}
			if len(jobs) == 0 {
				// An ambiguous result may have committed; those jobs then
				// recover by lease expiry. Nothing is held locally.
				if ctx.Err() == nil {
					r.park(ctx, nil, claimErrorWait)
				}
				continue
			}
			// One statement of a multi-statement claim failed after another
			// committed. The jobs known to be ours still run.
		}

		for i, job := range jobs {
			inFlight.Add(1)
			select {
			case work <- job:
			case <-ctx.Done():
				// Hand the rest back through a fenced transition rather than
				// holding them while their leases run.
				inFlight.Add(-1)
				r.giveBack(workCtx, jobs[i:])
				return
			}
		}
		if len(jobs) < n {
			// The queue ran dry for now: wait for a notification or a due time
			// rather than asking again immediately.
			r.idle(ctx)
		}
	}
}

// giveBack returns claimed-but-unstarted jobs and their gate slots.
func (r *Runtime) giveBack(workCtx context.Context, jobs []postgres.Job) {
	for _, job := range jobs {
		r.verdict(workCtx, job, "return unstarted claim", func(ctx context.Context) error {
			return r.Queue.Release(ctx, job, 0)
		})
		r.Gate.Release(1)
	}
}

// park waits for the context, a signal, or the timer. A nil signal blocks only
// on the other two.
func (r *Runtime) park(ctx context.Context, signal <-chan struct{}, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-signal:
	case <-t.C:
	}
}

// idle waits after an empty claim: for a notification, the known next due
// time, or the fallback timer, whichever is first. A due hint that has already
// passed is consumed, not obeyed — the claim that just ran saw whatever it
// described.
func (r *Runtime) idle(ctx context.Context) {
	d := pollFallback
	if due := r.due.Load(); due != 0 {
		if until := time.Until(time.Unix(0, due)); until > 0 {
			d = min(d, until+10*time.Millisecond)
		} else {
			r.consumed.Store(due)
			r.due.CompareAndSwap(due, 0)
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-r.wake:
	case <-t.C:
	}
}

// startHeartbeat keeps a running handler's lease alive until stopped.
//
// Renewal runs on its own connection and a short deadline, never on the
// handler's transaction. Zero rows is confirmed loss and cancels the handler
// at once. A database error leaves ownership uncertain, so renewal is retried
// with jitter while a conservative local estimate says the lease still holds:
// the estimate is the request's start time plus the remaining duration the
// database reported, so response delay can only make it earlier. Only when no
// confirmation arrives before that estimate minus the safety margin is the
// handler cancelled, and its claim is left to expire with its attempt charged.
func (r *Runtime) startHeartbeat(job postgres.Job, cancel context.CancelCauseFunc) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	expiry := job.Started.Add(job.LeaseUntil.Sub(job.ServerNow))
	budgetEnd := job.Started.Add(postgres.HandlerBudget)

	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(renewEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
			}
			for {
				now := time.Now()
				limit := expiry.Add(-renewMargin)
				if budgetEnd.Before(limit) {
					limit = budgetEnd
				}
				if !now.Before(limit) {
					r.Log.Warn("lease unconfirmed inside the safety window; cancelling the handler",
						"worker_type", r.Name, "job_id", job.ID)
					cancel(errLeaseUncertain)
					return
				}
				qctx, qcancel := context.WithTimeout(context.Background(), min(postgres.ControlTimeout, limit.Sub(now)))
				lease, err := r.Queue.Renew(qctx, job)
				qcancel()
				if err == nil {
					expiry = now.Add(lease.Until.Sub(lease.ServerNow))
					break
				}
				if errors.Is(err, postgres.ErrLeaseLost) {
					cancel(postgres.ErrLeaseLost)
					return
				}
				r.Log.Debug("lease renewal failed, will retry", "worker_type", r.Name, "job_id", job.ID, "error", err)
				wait := renewRetryMin + time.Duration(rand.Int64N(int64(renewRetryMax-renewRetryMin)+1))
				select {
				case <-done:
					return
				case <-time.After(wait):
				}
			}
		}
	}()

	var once sync.Once
	return func() { once.Do(func() { close(done); wg.Wait() }) }
}

// invoke runs the job with a panic guard.
//
// One process hosts every role. A panic that used to kill a single pod and be
// restarted by Kubernetes would take the whole pipeline down, so it is
// converted into a terminal failure for that one job instead.
func (r *Runtime) invoke(ctx context.Context, job postgres.Job, h Handler) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = Fatal(domain.ReasonHandlerPanic, fmt.Errorf("%v\n%s", p, debug.Stack()))
		}
	}()
	var declined error
	err = r.Queue.Run(ctx, job, func(ctx context.Context) error {
		err := h(ctx, job)
		var d *Declined
		if errors.As(err, &d) {
			if uerr := r.Docs.UpdateStatus(ctx, d.DocID, domain.StatusSkipped, d.Reason); uerr != nil {
				return uerr
			}
			declined = err
			return nil
		}
		return err
	})
	if err == nil && declined != nil {
		return declined
	}
	return err
}

func (r *Runtime) handle(workCtx context.Context, job postgres.Job, h Handler) {
	if r.Stats != nil {
		r.Stats.LaneStarted()
		defer r.Stats.LaneFinished()
	}
	if !job.Charged {
		// The budget was spent before this claim. It authorizes the failure
		// bookkeeping and never a further handler execution.
		r.failExhausted(workCtx, job)
		return
	}

	start := time.Now()
	// The handler's own deadline runs from claim time, kept separate from
	// workCtx because the bookkeeping below still has work to do and must not
	// inherit an expired or cancelled context.
	hctx, cancelCause := context.WithCancelCause(workCtx)
	hctx, cancelDeadline := context.WithDeadline(hctx, job.Started.Add(postgres.HandlerBudget))
	stopHeartbeat := r.startHeartbeat(job, cancelCause)
	err := r.invoke(hctx, job, h)
	cause := context.Cause(hctx)
	cancelDeadline()
	cancelCause(nil)
	// Join the heartbeat before settling, so a renewal racing a normal
	// completion cannot be misread as lease loss.
	stopHeartbeat()
	telemetry.IngestionDuration.WithLabelValues(r.Name, r.Subject).Observe(time.Since(start).Seconds())

	r.settle(workCtx, job, err, cause)
}

// settle turns a handler's result into at most one fenced verdict.
func (r *Runtime) settle(workCtx context.Context, job postgres.Job, err, cause error) {
	switch {
	case err == nil:
		telemetry.IngestionTasks.WithLabelValues(r.Name, "completed").Inc()
		return
	case isDeclined(err):
		var d *Declined
		errors.As(err, &d)
		telemetry.IngestionTasks.WithLabelValues(r.Name, "skipped").Inc()
		telemetry.Skipped.WithLabelValues(string(d.Reason)).Inc()
		r.Log.Info("declined", "reason", d.Reason, "doc_id", d.DocID)
		return
	}

	if errors.Is(err, postgres.ErrCommitUncertain) {
		ctx, cancel := bookkeeping(workCtx)
		st, serr := r.Queue.State(ctx, job)
		cancel()
		switch {
		case serr != nil:
			r.Log.Error("commit outcome unreadable; leaving the job to expire",
				"worker_type", r.Name, "job_id", job.ID, "error", serr)
			return
		case st.Exists && st.State == "COMPLETED" && st.Generation == job.Generation:
			telemetry.IngestionTasks.WithLabelValues(r.Name, "completed").Inc()
			return
		case !st.Exists || st.State != "RUNNING" || st.Generation != job.Generation:
			r.lostLease(job, "uncertain commit")
			return
		}
		// Still ours and still running: the commit did not happen, so this is
		// an ordinary transient failure.
	}

	if errors.Is(err, postgres.ErrLeaseLost) || errors.Is(cause, postgres.ErrLeaseLost) || errors.Is(cause, errLeaseUncertain) {
		r.lostLease(job, "handler outcome")
		return
	}

	if errors.Is(err, postgres.ErrJobBusy) {
		// An expired handler of this job is still physically running and holds
		// its advisory lock. Back off; the lock ends with its transaction.
		r.verdict(workCtx, job, "busy return", func(ctx context.Context) error {
			return r.Queue.Release(ctx, job, postgres.BusyDelay)
		})
		return
	}

	if errors.Is(err, source.ErrUnavailable) || errors.Is(err, source.ErrChanged) {
		reason := "SOURCE_UNAVAILABLE"
		if errors.Is(err, source.ErrChanged) {
			reason = "SOURCE_CHANGED"
		}
		if r.verdict(workCtx, job, "source wait", func(ctx context.Context) error {
			return r.Queue.Wait(ctx, job, reason)
		}) {
			r.Log.Warn("ingestion waiting for source", "reason", reason)
		}
		return
	}

	// Cancellation is not a verdict about the document. The job goes straight
	// back, with the attempt this claim charged refunded.
	if workCtx.Err() != nil {
		if r.verdict(workCtx, job, "shutdown return", func(ctx context.Context) error {
			return r.Queue.Release(ctx, job, 0)
		}) {
			telemetry.IngestionTasks.WithLabelValues(r.Name, "retry").Inc()
			r.Log.Info("work returned unfinished: the run was shutting down",
				"worker_type", r.Name, "error", err)
		}
		return
	}

	// A job can outlive its document. Pruning removes what the workspace no
	// longer stages, and a job enqueued before a sweep can name a document the
	// sweep has since deleted. Everything downstream then fails for a reason
	// that is not the work's fault, and failing it would leave an entry whose
	// document an operator cannot go and look at. The job row went with the
	// document, so there is nothing to settle. Asked only here, on a path that
	// has already failed, so the ordinary case pays nothing.
	if id := docIDOf(err); id != "" && r.Docs != nil {
		if exists, cerr := r.Docs.Exists(workCtx, id); cerr == nil && !exists {
			telemetry.IngestionTasks.WithLabelValues(r.Name, "skipped").Inc()
			telemetry.Skipped.WithLabelValues(string(domain.ReasonDocumentPruned)).Inc()
			r.Log.Info("work discarded: its document was pruned", "doc_id", id, "worker_type", r.Name)
			return
		}
	}

	reason := reasonOf(err)
	if isTerminal(err) || job.Attempts >= postgres.MaxAttempts {
		if r.fail(workCtx, job, reason) {
			r.Log.Error("terminal failure, job failed",
				"error", err, "attempt", job.Attempts, "max", postgres.MaxAttempts, "reason", reason)
		}
		return
	}

	delay := nakBackoff(job.Attempts)
	if r.verdict(workCtx, job, "retry", func(ctx context.Context) error {
		return r.Queue.Retry(ctx, job, delay, reason)
	}) {
		telemetry.IngestionTasks.WithLabelValues(r.Name, "retry").Inc()
		r.Log.Warn("transient failure, will retry",
			"error", err, "attempt", job.Attempts, "max", postgres.MaxAttempts, "retry_in", delay)
	}
}

// failExhausted fails a job whose retry budget was spent before this claim.
func (r *Runtime) failExhausted(workCtx context.Context, job postgres.Job) {
	reason := job.LastFailure
	if reason == "" {
		reason = domain.ReasonUnclassified
	}
	if r.fail(workCtx, job, reason) {
		r.Log.Error("retry budget exhausted; job failed without running its handler",
			"worker_type", r.Name, "job_id", job.ID, "reason", reason)
	}
}

// fail records the FAILED verdict and counts it once. When the outcome is
// ambiguous, durable state decides whether it was committed.
func (r *Runtime) fail(workCtx context.Context, job postgres.Job, reason domain.FailureReason) bool {
	ctx, cancel := bookkeeping(workCtx)
	err := r.Queue.Fail(ctx, job, reason)
	cancel()
	switch {
	case err == nil:
	case errors.Is(err, postgres.ErrLeaseLost):
		r.lostLease(job, "failure verdict")
		return false
	case errors.Is(err, postgres.ErrJobBusy):
		// An expired handler of this job still holds its advisory lock. Wait it
		// out like any busy return; an exhausted claim keeps its spent budget.
		r.verdict(workCtx, job, "busy failure return", func(ctx context.Context) error {
			return r.Queue.Release(ctx, job, postgres.BusyDelay)
		})
		return false
	default:
		ctx, cancel := bookkeeping(workCtx)
		st, serr := r.Queue.State(ctx, job)
		cancel()
		if serr != nil || !st.Exists || st.State != "FAILED" || st.Generation != job.Generation {
			r.Log.Error("failure verdict not recorded; the job will expire and be reclaimed",
				"worker_type", r.Name, "job_id", job.ID, "error", err)
			return false
		}
	}
	telemetry.IngestionTasks.WithLabelValues(r.Name, "dlq").Inc()
	telemetry.DLQ.WithLabelValues(r.Name, string(reason)).Inc()
	return true
}

// verdict records one fenced transition under a fresh context independent of
// handler and shutdown cancellation. It never issues a second transition: a
// failure is logged and the job is left for expiry to resolve.
func (r *Runtime) verdict(workCtx context.Context, job postgres.Job, what string, fn func(context.Context) error) bool {
	ctx, cancel := bookkeeping(workCtx)
	defer cancel()
	err := fn(ctx)
	switch {
	case err == nil:
		return true
	case errors.Is(err, postgres.ErrLeaseLost):
		r.lostLease(job, what)
	default:
		r.Log.Error("verdict not recorded; the job will expire and be reclaimed",
			"worker_type", r.Name, "job_id", job.ID, "verdict", what, "error", err)
	}
	return false
}

func (r *Runtime) lostLease(job postgres.Job, what string) {
	telemetry.LeasesLost.WithLabelValues(r.Name).Inc()
	r.Log.Warn("job ownership lost; no verdict recorded",
		"worker_type", r.Name, "job_id", job.ID, "during", what)
}

// bookkeeping is a fresh context for a verdict: workCtx's values, none of its
// cancellation, and the control deadline.
func bookkeeping(workCtx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(workCtx), postgres.ControlTimeout)
}

// nakBackoff spaces a retry out instead of asking for it immediately.
//
// An immediate retry is only the right answer when the next attempt has some
// reason to go better, and under a failing dependency it has none. The
// breaker in front of the embedding endpoint refuses instantly for thirty
// seconds after it trips, so a job retried without delay is claimed, refused
// and failed again in the time it takes to make three round trips. The
// attempt budget then reads three refusals in a few milliseconds as a document
// that cannot be processed, which is how 494 perfectly good documents
// failed during one overloaded half hour — the failure was in front of them,
// and they were punished for arriving during it.
//
// The delay therefore has to outlast the cooldown of whatever is failing.
// The jitter is not decoration at this corpus's scale: 49,000 jobs that
// fail together would otherwise come back together and re-overload the thing
// they were waiting for, which is the stampede that tripped the breaker in
// the first place.
func nakBackoff(attempts int) time.Duration {
	d := nakBackoffBase
	for i := 1; i < attempts && d < nakBackoffMax; i++ {
		d *= 2
	}
	if d > nakBackoffMax {
		d = nakBackoffMax
	}
	// Half fixed, half spread: every retry waits out the cooldown, and no two
	// arrive at the same instant.
	return d/2 + time.Duration(rand.Int64N(int64(d/2)))
}

func isDeclined(err error) bool {
	var d *Declined
	return errors.As(err, &d)
}

func isTerminal(err error) bool {
	var t *Terminal
	return errors.As(err, &t)
}

func reasonOf(err error) domain.FailureReason {
	var t *Terminal
	if errors.As(err, &t) {
		return t.Reason
	}
	// Deliberately not ReasonExtractionFailed. Defaulting to a real class makes
	// every unclassified error indistinguishable from a genuine extraction
	// failure, so a Tier 1 read outage reads as a corpus full of broken
	// documents. UNCLASSIFIED says the true thing: nobody named this path yet.
	return domain.ReasonUnclassified
}

// docIDErr lets a handler attach the document a failure belongs to, so the
// Tier 2 row can be marked FAILED alongside the job.
type docIDErr struct {
	DocID string
	Err   error
}

func (d *docIDErr) Error() string { return fmt.Sprintf("doc %s: %v", d.DocID, d.Err) }
func (d *docIDErr) Unwrap() error { return d.Err }

// WithDoc annotates an error with its document id.
func WithDoc(docID string, err error) error {
	if err == nil {
		return nil
	}
	return &docIDErr{DocID: docID, Err: err}
}

func docIDOf(err error) string {
	var d *docIDErr
	if errors.As(err, &d) {
		return d.DocID
	}
	return ""
}
