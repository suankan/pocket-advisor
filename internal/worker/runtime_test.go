package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/source"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/telemetry"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// --- fakes -----------------------------------------------------------------

type verdictCall struct {
	Kind   string
	Job    postgres.Job
	Delay  time.Duration
	Reason string
}

// fakeQueue is an in-memory Queue. It records every verdict so each test can
// assert exactly which one was issued, and none other.
type fakeQueue struct {
	mu      sync.Mutex
	pending []postgres.Job
	claims  []int // the limit of every Claim call
	// claimErr, when set, is returned by Claim.
	claimErr error
	// claimErrAfterJobs, when set, makes Claim return the jobs it took together
	// with this error: one statement of a multi-statement claim committed and a
	// later one failed.
	claimErrAfterJobs error
	// runErr, when set, is returned by Run before the handler executes.
	runErr func(postgres.Job) error
	// renew answers a lease renewal; the default extends by five minutes.
	renew func(context.Context, postgres.Job) (postgres.Lease, error)
	// state answers State; the default reports the job as missing.
	state func(postgres.Job) postgres.JobState
	// failErr, retryErr and releaseErr are returned by the matching verdict.
	failErr, retryErr, releaseErr error

	verdicts []verdictCall
	renewals atomic.Int64
	runs     atomic.Int64
}

func (q *fakeQueue) enqueue(jobs ...postgres.Job) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending = append(q.pending, jobs...)
}

func (q *fakeQueue) Claim(_ context.Context, _ string, limit int) ([]postgres.Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.claims = append(q.claims, limit)
	if q.claimErr != nil {
		return nil, q.claimErr
	}
	n := min(limit, len(q.pending))
	out := append([]postgres.Job(nil), q.pending[:n]...)
	q.pending = q.pending[n:]
	if q.claimErrAfterJobs != nil && len(out) > 0 {
		return out, q.claimErrAfterJobs
	}
	return out, nil
}

func (q *fakeQueue) Renew(ctx context.Context, j postgres.Job) (postgres.Lease, error) {
	q.renewals.Add(1)
	if q.renew != nil {
		return q.renew(ctx, j)
	}
	now := time.Now()
	return postgres.Lease{Until: now.Add(postgres.LeaseDuration), ServerNow: now}, nil
}

func (q *fakeQueue) Run(ctx context.Context, j postgres.Job, fn func(context.Context) error) error {
	q.runs.Add(1)
	if q.runErr != nil {
		if err := q.runErr(j); err != nil {
			return err
		}
	}
	return fn(ctx)
}

func (q *fakeQueue) State(_ context.Context, j postgres.Job) (postgres.JobState, error) {
	if q.state != nil {
		return q.state(j), nil
	}
	return postgres.JobState{}, nil
}

func (q *fakeQueue) record(v verdictCall) {
	q.mu.Lock()
	q.verdicts = append(q.verdicts, v)
	q.mu.Unlock()
}

func (q *fakeQueue) Retry(_ context.Context, j postgres.Job, delay time.Duration, reason domain.FailureReason) error {
	q.record(verdictCall{"retry", j, delay, string(reason)})
	return q.retryErr
}
func (q *fakeQueue) Release(_ context.Context, j postgres.Job, delay time.Duration) error {
	q.record(verdictCall{"release", j, delay, ""})
	return q.releaseErr
}
func (q *fakeQueue) Wait(_ context.Context, j postgres.Job, reason string) error {
	q.record(verdictCall{"wait", j, 0, reason})
	return nil
}
func (q *fakeQueue) Fail(_ context.Context, j postgres.Job, reason domain.FailureReason) error {
	q.record(verdictCall{"fail", j, 0, string(reason)})
	return q.failErr
}

func (q *fakeQueue) kinds() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []string
	for _, v := range q.verdicts {
		out = append(out, v.Kind)
	}
	return out
}

func (q *fakeQueue) only(t *testing.T, kind string) verdictCall {
	t.Helper()
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.verdicts) != 1 || q.verdicts[0].Kind != kind {
		t.Fatalf("verdicts = %+v, want exactly one %q", q.verdicts, kind)
	}
	return q.verdicts[0]
}

func (q *fakeQueue) none(t *testing.T) {
	t.Helper()
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.verdicts) != 0 {
		t.Fatalf("verdicts = %+v, want none", q.verdicts)
	}
}

// fakeDocs answers the two questions the runtime asks Tier 2.
type fakeDocs struct {
	missing   map[string]bool
	statuses  atomic.Int64
	existsErr error
}

func (f *fakeDocs) UpdateStatus(context.Context, string, domain.Status, domain.FailureReason) error {
	f.statuses.Add(1)
	return nil
}
func (f *fakeDocs) Exists(_ context.Context, docID string) (bool, error) {
	if f.existsErr != nil {
		return false, f.existsErr
	}
	return !f.missing[docID], nil
}

var jobSeq atomic.Int64

// newJob is a freshly claimed, charged job whose lease has five minutes left.
func newJob(attempts int) postgres.Job {
	n := jobSeq.Add(1)
	now := time.Now()
	return postgres.Job{
		ID: fmt.Sprintf("job-%d", n), DocID: fmt.Sprintf("doc-%d", n), Subject: "test.subject",
		Payload: []byte("payload"), Generation: 2, Attempts: attempts, Charged: true,
		ClaimedAt: now, LeaseUntil: now.Add(postgres.LeaseDuration), ServerNow: now, Started: now,
	}
}

func newRuntime(q *fakeQueue, lanes int) *Runtime {
	return &Runtime{
		Queue: q, Gate: postgres.NewGate(max(lanes, 1)), Name: "test", Subject: "test.subject",
		Log: quietLogger(), Lanes: lanes, Docs: &fakeDocs{},
	}
}

// consume runs the pool until done() reports true, then stops claiming and
// lets in-flight work finish, as the CLI's first interrupt does.
func consume(t *testing.T, rt *Runtime, h Handler, done func() bool) {
	t.Helper()
	fetchCtx, stop := context.WithCancel(context.Background())
	defer stop()
	finished := make(chan error, 1)
	go func() { finished <- rt.Consume(fetchCtx, context.Background(), h) }()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			stop()
			<-finished
			t.Fatal("timed out waiting for the pool")
		}
		time.Sleep(time.Millisecond)
	}
	stop()
	if err := <-finished; err != nil {
		t.Fatalf("Consume: %v", err)
	}
}

// shortTimings makes every queue timing small for the duration of a test.
func shortTimings(t *testing.T) {
	t.Helper()
	oe, om, rmin, rmax, pf, cw := renewEvery, renewMargin, renewRetryMin, renewRetryMax, pollFallback, claimErrorWait
	renewEvery, renewMargin = 5*time.Millisecond, 20*time.Millisecond
	renewRetryMin, renewRetryMax = time.Millisecond, 3*time.Millisecond
	pollFallback, claimErrorWait = 40*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() {
		renewEvery, renewMargin, renewRetryMin, renewRetryMax, pollFallback, claimErrorWait = oe, om, rmin, rmax, pf, cw
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// --- the pool --------------------------------------------------------------

// The pool is the component the whole refactor rests on: "does it actually run
// N at once, and does it stay at N" is the property worth pinning down.
func TestConsumeRunsLanesConcurrentlyAndRespectsTheBound(t *testing.T) {
	const lanes, jobs = 8, 60
	q := &fakeQueue{}
	for i := 0; i < jobs; i++ {
		q.enqueue(newJob(1))
	}
	rt := newRuntime(q, lanes)

	var mu sync.Mutex
	concurrent, peak := 0, 0
	var handled atomic.Int64
	consume(t, rt, func(context.Context, postgres.Job) error {
		mu.Lock()
		concurrent++
		peak = max(peak, concurrent)
		mu.Unlock()
		time.Sleep(3 * time.Millisecond)
		mu.Lock()
		concurrent--
		mu.Unlock()
		handled.Add(1)
		return nil
	}, func() bool { return handled.Load() == jobs })

	if peak > lanes {
		t.Errorf("peak concurrency %d exceeded Lanes=%d", peak, lanes)
	}
	if peak < 2 {
		t.Errorf("peak concurrency %d — the pool is still serialising", peak)
	}
	if got := q.runs.Load(); got != jobs {
		t.Errorf("ran %d jobs, want %d", got, jobs)
	}
	q.none(t) // successful jobs need no verdict beyond their own commit
}

// A claim must never exceed what the pool can start right now: a job held in a
// buffer keeps its lease running while it waits.
func TestConsumeNeverClaimsMoreThanIdleLanesOrFreeGateSlots(t *testing.T) {
	const lanes = 6
	q := &fakeQueue{}
	for i := 0; i < 40; i++ {
		q.enqueue(newJob(1))
	}
	rt := newRuntime(q, lanes)
	rt.Gate = postgres.NewGate(3) // fewer slots than lanes
	var handed atomic.Int64
	consume(t, rt, func(context.Context, postgres.Job) error {
		handed.Add(1)
		time.Sleep(2 * time.Millisecond)
		return nil
	}, func() bool { return handed.Load() == 40 })

	q.mu.Lock()
	defer q.mu.Unlock()
	for _, limit := range q.claims {
		if limit > 3 {
			t.Fatalf("claimed %d jobs with 3 gate slots and %d lanes", limit, lanes)
		}
	}
	if rt.Gate.Free() != 3 {
		t.Errorf("gate holds %d free slots at rest, want 3: a slot leaked", rt.Gate.Free())
	}
}

// Two pools sharing one gate must never hold more than it has, and neither may
// sit on a partial batch while waiting for the rest.
func TestCompetingPoolsShareTheGateWithoutHoldingPartialBatches(t *testing.T) {
	gate := postgres.NewGate(3)
	var inFlight, peak atomic.Int64
	handler := func(context.Context, postgres.Job) error {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		inFlight.Add(-1)
		return nil
	}
	var wg sync.WaitGroup
	var handled atomic.Int64
	counted := func(ctx context.Context, j postgres.Job) error { err := handler(ctx, j); handled.Add(1); return err }
	fetchCtx, stop := context.WithCancel(context.Background())
	defer stop()
	for p := 0; p < 2; p++ {
		q := &fakeQueue{}
		for i := 0; i < 30; i++ {
			q.enqueue(newJob(1))
		}
		rt := newRuntime(q, 8)
		rt.Gate = gate
		wg.Add(1)
		go func() { defer wg.Done(); _ = rt.Consume(fetchCtx, context.Background(), counted) }()
	}
	waitFor(t, "both pools to finish their work", func() bool { return handled.Load() == 60 })
	stop()
	wg.Wait()
	if peak.Load() > 3 {
		t.Fatalf("%d handlers ran at once through a gate of 3", peak.Load())
	}
	if gate.Free() != 3 {
		t.Fatalf("gate has %d free slots at rest, want 3", gate.Free())
	}
}

// A panic used to kill one pod and be restarted by Kubernetes. In one process
// it would take every role down, so it has to become a terminal failure for
// that job alone while the pool keeps running.
func TestConsumeRecoversFromHandlerPanic(t *testing.T) {
	q := &fakeQueue{}
	for i := 0; i < 6; i++ {
		q.enqueue(newJob(1))
	}
	rt := newRuntime(q, 2)
	var seen atomic.Int64
	consume(t, rt, func(context.Context, postgres.Job) error {
		if seen.Add(1)%2 == 0 {
			panic("handler exploded")
		}
		return nil
	}, func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()
		return seen.Load() == 6 && len(q.verdicts) == 3
	})
	for _, v := range q.verdicts {
		if v.Kind != "fail" || v.Reason != string(domain.ReasonHandlerPanic) {
			t.Errorf("verdict %+v, want a HANDLER_PANIC failure", v)
		}
	}
	if got := q.runs.Load(); got != 6 {
		t.Errorf("only %d of 6 jobs ran: the pool stopped after a panic", got)
	}
}

// The first Ctrl+C stops claiming but must let in-flight work finish and
// complete normally.
func TestConsumeDrainsInFlightWorkAfterClaimingStops(t *testing.T) {
	q := &fakeQueue{}
	for i := 0; i < 2; i++ {
		q.enqueue(newJob(1))
	}
	rt := newRuntime(q, 2)
	started := make(chan struct{}, 2)
	var completed atomic.Int64

	fetchCtx, stopFetch := context.WithCancel(context.Background())
	go func() {
		<-started
		stopFetch() // stop claiming while a handler is provably mid-flight
	}()
	if err := rt.Consume(fetchCtx, context.Background(), func(context.Context, postgres.Job) error {
		started <- struct{}{}
		time.Sleep(50 * time.Millisecond)
		completed.Add(1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if completed.Load() == 0 {
		t.Fatal("in-flight work was abandoned instead of drained")
	}
	// A claim that never reached a lane before claiming stopped is legitimately
	// handed back; nothing else may be recorded for work that ran.
	for _, kind := range q.kinds() {
		if kind != "release" {
			t.Fatalf("verdicts = %v: drained work must record none, only unstarted claims may be released", q.kinds())
		}
	}
	if int64(len(q.kinds()))+completed.Load() != 2 {
		t.Fatalf("%d completed and %d returned of 2 claimed jobs", completed.Load(), len(q.kinds()))
	}
}

// A claimed job that never reaches a lane goes back through a fenced
// transition with its attempt refunded, and its gate slot returns.
func TestGiveBackReleasesClaimsAndSlots(t *testing.T) {
	q := &fakeQueue{}
	rt := newRuntime(q, 4)
	if rt.Gate.TryReserve(3) != 3 {
		t.Fatal("could not reserve")
	}
	rt.giveBack(context.Background(), []postgres.Job{newJob(1), newJob(2), newJob(1)})
	if got := q.kinds(); len(got) != 3 || got[0] != "release" {
		t.Fatalf("verdicts = %v, want three releases", got)
	}
	for _, v := range q.verdicts {
		if v.Delay != 0 {
			t.Errorf("an unstarted claim was returned with a delay of %v", v.Delay)
		}
	}
	if rt.Gate.Free() != 4 {
		t.Errorf("gate free = %d, want 4", rt.Gate.Free())
	}
}

// --- idle behaviour --------------------------------------------------------

func TestIdlePoolDoesNotSpin(t *testing.T) {
	shortTimings(t)
	q := &fakeQueue{}
	rt := newRuntime(q, 4)
	fetchCtx, stop := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer stop()
	if err := rt.Consume(fetchCtx, context.Background(), func(context.Context, postgres.Job) error { return nil }); err != nil {
		t.Fatal(err)
	}
	q.mu.Lock()
	n := len(q.claims)
	q.mu.Unlock()
	// 400ms at a 40ms fallback is about ten claims; a spinning loop is thousands.
	if n < 2 || n > 25 {
		t.Fatalf("%d empty claims in 400ms, want a poll every fallback interval", n)
	}
	if rt.Gate.Free() != 4 {
		t.Fatalf("idle reservations retained: %d free of 4", rt.Gate.Free())
	}
}

func TestFullGateParksInsteadOfSpinning(t *testing.T) {
	shortTimings(t)
	q := &fakeQueue{}
	q.enqueue(newJob(1))
	rt := newRuntime(q, 2)
	if rt.Gate.TryReserve(rt.Gate.Size()) != 2 {
		t.Fatal("could not fill the gate")
	}
	fetchCtx, stop := context.WithCancel(context.Background())
	defer stop()
	finished := make(chan struct{})
	var handled atomic.Int64
	go func() {
		_ = rt.Consume(fetchCtx, context.Background(), func(context.Context, postgres.Job) error { handled.Add(1); return nil })
		close(finished)
	}()
	time.Sleep(300 * time.Millisecond)
	q.mu.Lock()
	claims := len(q.claims)
	q.mu.Unlock()
	if claims != 0 {
		t.Fatalf("%d claims were issued against a full gate", claims)
	}
	rt.Gate.Release(2) // capacity returns: the pool must wake and claim
	waitFor(t, "the pool to claim once capacity returned", func() bool { return handled.Load() == 1 })
	stop()
	<-finished
}

func TestWakeAndDueHintsBringTheNextLookForward(t *testing.T) {
	shortTimings(t)
	pollFallback = time.Hour // only a wake, a hint or shutdown can end the wait
	q := &fakeQueue{}
	rt := newRuntime(q, 2)
	fetchCtx, stop := context.WithCancel(context.Background())
	defer stop()
	finished := make(chan struct{})
	go func() {
		_ = rt.Consume(fetchCtx, context.Background(), func(context.Context, postgres.Job) error { return nil })
		close(finished)
	}()
	claimCount := func() int { q.mu.Lock(); defer q.mu.Unlock(); return len(q.claims) }
	waitFor(t, "the first claim", func() bool { return claimCount() == 1 })
	time.Sleep(50 * time.Millisecond)
	if claimCount() != 1 {
		t.Fatalf("an idle pool claimed again with no signal: %d", claimCount())
	}

	rt.Wake()
	waitFor(t, "a claim after a wake", func() bool { return claimCount() == 2 })

	// A due time in the near future schedules a look at that instant.
	rt.SetDue(time.Now().Add(60 * time.Millisecond))
	waitFor(t, "a claim at the hinted due time", func() bool { return claimCount() == 3 })

	// A hint already in the past is consumed once, not obeyed repeatedly.
	past := time.Now().Add(-time.Second)
	rt.SetDue(past)
	waitFor(t, "the one look the hint earns", func() bool { return claimCount() == 4 })
	before := claimCount()
	for i := 0; i < 20; i++ {
		rt.SetDue(past) // the sampler republishing the same stale hint
		time.Sleep(5 * time.Millisecond)
	}
	if got := claimCount(); got > before+1 {
		t.Fatalf("a stale due hint scheduled %d immediate claims", got-before)
	}
	stop()
	<-finished
}

// If one statement of a multi-statement claim commits and a later one fails,
// the jobs already claimed are ours: dropping them would leave them RUNNING
// until their leases expire for no reason.
func TestPartialClaimFailureStillRunsTheJobsItTook(t *testing.T) {
	shortTimings(t)
	q := &fakeQueue{claimErrAfterJobs: errors.New("second statement failed")}
	for i := 0; i < 3; i++ {
		q.enqueue(newJob(1))
	}
	rt := newRuntime(q, 3)
	var handled atomic.Int64
	consume(t, rt, func(context.Context, postgres.Job) error { handled.Add(1); return nil },
		func() bool { return handled.Load() == 3 })
	if rt.Gate.Free() != 3 {
		t.Errorf("gate free = %d, want 3: a slot leaked on a partial claim", rt.Gate.Free())
	}
}

func TestClaimErrorsBackOffAndHoldNothing(t *testing.T) {
	shortTimings(t)
	q := &fakeQueue{claimErr: errors.New("connection reset")}
	rt := newRuntime(q, 3)
	fetchCtx, stop := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer stop()
	if err := rt.Consume(fetchCtx, context.Background(), func(context.Context, postgres.Job) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if rt.Gate.Free() != 3 {
		t.Fatalf("a failed claim leaked gate slots: %d free of 3", rt.Gate.Free())
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.claims) < 2 || len(q.claims) > 40 {
		t.Fatalf("%d claim attempts in 200ms, want a paced retry", len(q.claims))
	}
}

// --- outcomes --------------------------------------------------------------

func settleOnce(t *testing.T, q *fakeQueue, rt *Runtime, job postgres.Job, h Handler) {
	t.Helper()
	rt.handle(context.Background(), job, h)
}

func TestTransientFailureIsRetriedWithADelayAndItsReason(t *testing.T) {
	q := &fakeQueue{}
	rt := newRuntime(q, 1)
	job := newJob(1)
	settleOnce(t, q, rt, job, func(context.Context, postgres.Job) error { return errors.New("dependency unavailable") })
	v := q.only(t, "retry")
	if v.Reason != string(domain.ReasonUnclassified) {
		t.Errorf("reason = %q, want UNCLASSIFIED for an unnamed error", v.Reason)
	}
	// The delay must outlast the embedding breaker's cooldown.
	if v.Delay < 30*time.Second {
		t.Errorf("retry delay %v is inside the breaker cooldown: the retry is certain to be refused", v.Delay)
	}
}

func TestThirdTransientFailureFailsTheJob(t *testing.T) {
	q := &fakeQueue{}
	rt := newRuntime(q, 1)
	job := newJob(postgres.MaxAttempts)
	before := testutil.ToFloat64(telemetry.DLQ.WithLabelValues("test", string(domain.ReasonUnclassified)))
	settleOnce(t, q, rt, job, func(context.Context, postgres.Job) error { return errors.New("still unavailable") })
	q.only(t, "fail")
	if got := testutil.ToFloat64(telemetry.DLQ.WithLabelValues("test", string(domain.ReasonUnclassified))); got != before+1 {
		t.Errorf("failed-outcome counter moved by %v, want 1", got-before)
	}
}

func TestTerminalFailureFailsImmediatelyWithItsReason(t *testing.T) {
	q := &fakeQueue{}
	rt := newRuntime(q, 1)
	settleOnce(t, q, rt, newJob(1), func(context.Context, postgres.Job) error {
		return Fatal(domain.ReasonMalformedCommand, errors.New("bad payload"))
	})
	if v := q.only(t, "fail"); v.Reason != string(domain.ReasonMalformedCommand) {
		t.Errorf("reason = %q", v.Reason)
	}
}

// An exhausted claim authorizes failure bookkeeping, never a fourth execution.
func TestExhaustedClaimFailsWithoutRunningTheHandler(t *testing.T) {
	for name, tc := range map[string]struct {
		last domain.FailureReason
		want string
	}{
		"a recorded reason":   {domain.ReasonOCRFailed, "OCR_FAILED"},
		"no recorded verdict": {"", "UNCLASSIFIED"},
	} {
		q := &fakeQueue{}
		rt := newRuntime(q, 1)
		job := newJob(postgres.MaxAttempts)
		job.Charged, job.LastFailure = false, tc.last
		rt.handle(context.Background(), job, func(context.Context, postgres.Job) error {
			t.Errorf("%s: the handler ran a fourth time", name)
			return nil
		})
		if v := q.only(t, "fail"); v.Reason != tc.want {
			t.Errorf("%s: reason = %q, want %q", name, v.Reason, tc.want)
		}
		if q.runs.Load() != 0 {
			t.Errorf("%s: Run was entered for an exhausted claim", name)
		}
	}
}

// Cancellation is not a verdict about the document: it must return the job,
// even on the attempt that would otherwise have failed it.
func TestShutdownCancellationReturnsTheJobInsteadOfFailingIt(t *testing.T) {
	q := &fakeQueue{}
	rt := newRuntime(q, 1)
	workCtx, cancel := context.WithCancel(context.Background())
	cancel()
	rt.handle(workCtx, newJob(postgres.MaxAttempts), func(context.Context, postgres.Job) error {
		return errors.New(`stat "raw/69/6934": context canceled`)
	})
	if v := q.only(t, "release"); v.Delay != 0 {
		t.Errorf("shutdown return delayed by %v, want immediate", v.Delay)
	}
}

// A failure verdict that meets an expired handler still holding the job's
// advisory lock is a busy return: wait it out, and keep the spent budget of an
// exhausted claim rather than failing the job under a live handler.
func TestFailureVerdictBlockedByAStaleHandlerReturnsBusy(t *testing.T) {
	q := &fakeQueue{failErr: postgres.ErrJobBusy}
	rt := newRuntime(q, 1)
	job := newJob(postgres.MaxAttempts)
	job.Charged = false // the budget was spent before this claim
	rt.handle(context.Background(), job, func(context.Context, postgres.Job) error {
		t.Fatal("the handler ran for an exhausted claim")
		return nil
	})
	if kinds := q.kinds(); len(kinds) != 2 || kinds[0] != "fail" || kinds[1] != "release" {
		t.Fatalf("verdicts = %v, want the refused failure then a busy release", kinds)
	}
	if v := q.verdicts[1]; v.Delay != postgres.BusyDelay || v.Job.Charged {
		t.Errorf("busy release = %+v, want a thirty-second wait with no refund owed", v)
	}
}

func TestBusyReturnWaitsOutTheStaleHandler(t *testing.T) {
	q := &fakeQueue{runErr: func(postgres.Job) error { return postgres.ErrJobBusy }}
	rt := newRuntime(q, 1)
	rt.handle(context.Background(), newJob(2), func(context.Context, postgres.Job) error {
		t.Fatal("the handler ran despite a busy advisory lock")
		return nil
	})
	if v := q.only(t, "release"); v.Delay != postgres.BusyDelay {
		t.Errorf("busy return delay = %v, want %v", v.Delay, postgres.BusyDelay)
	}
}

// A deadlock or serialization abort is a race lost to another transaction.
// It returns the job after a short wait with its attempt refunded, even on the
// claim that would otherwise have failed it.
func TestTransactionConflictReturnsTheJobWithoutSpendingAnAttempt(t *testing.T) {
	for _, code := range []string{"40P01", "40001"} {
		q := &fakeQueue{}
		rt := newRuntime(q, 1)
		rt.handle(context.Background(), newJob(postgres.MaxAttempts), func(context.Context, postgres.Job) error {
			return WithDoc("doc", fmt.Errorf("create stub: %w", &pgconn.PgError{Code: code}))
		})
		v := q.only(t, "release")
		if v.Delay < postgres.ConflictDelay/2 || v.Delay >= postgres.ConflictDelay {
			t.Errorf("%s: conflict return delay = %v, want within [%v, %v)", code, v.Delay, postgres.ConflictDelay/2, postgres.ConflictDelay)
		}
		if !v.Job.Charged {
			t.Errorf("%s: the returned claim owes no refund", code)
		}
	}
}

func TestMissingOrChangedSourcesWaitForTheOriginal(t *testing.T) {
	for want, err := range map[string]error{
		"SOURCE_UNAVAILABLE": fmt.Errorf("open: %w", source.ErrUnavailable),
		"SOURCE_CHANGED":     fmt.Errorf("verify: %w", source.ErrChanged),
	} {
		q := &fakeQueue{}
		rt := newRuntime(q, 1)
		rt.handle(context.Background(), newJob(1), func(context.Context, postgres.Job) error { return err })
		if v := q.only(t, "wait"); v.Reason != want {
			t.Errorf("reason = %q, want %q", v.Reason, want)
		}
	}
}

func TestLostOwnershipRecordsNoVerdict(t *testing.T) {
	q := &fakeQueue{runErr: func(postgres.Job) error { return postgres.ErrLeaseLost }}
	rt := newRuntime(q, 1)
	before := testutil.ToFloat64(telemetry.LeasesLost.WithLabelValues("test"))
	rt.handle(context.Background(), newJob(1), func(context.Context, postgres.Job) error { return nil })
	q.none(t)
	if got := testutil.ToFloat64(telemetry.LeasesLost.WithLabelValues("test")); got != before+1 {
		t.Errorf("lost-lease counter moved by %v, want 1", got-before)
	}
}

func TestADeclineCompletesTheJobAndSkipsTheDocument(t *testing.T) {
	q := &fakeQueue{}
	docs := &fakeDocs{}
	rt := newRuntime(q, 1)
	rt.Docs = docs
	rt.handle(context.Background(), newJob(1), func(context.Context, postgres.Job) error {
		return Decline("doc-x", domain.ReasonUnsupportedFormat)
	})
	q.none(t) // the decline commits with the job's own completion
	if docs.statuses.Load() != 1 {
		t.Errorf("document status updated %d times, want once", docs.statuses.Load())
	}
}

// Pruning and the queue are not transactional with each other, so work claimed
// before a sweep can name a document the sweep has since deleted. That is not a
// failure of the work.
func TestWorkForAPrunedDocumentIsDiscardedNotFailed(t *testing.T) {
	q := &fakeQueue{}
	rt := newRuntime(q, 1)
	rt.Docs = &fakeDocs{missing: map[string]bool{"gone-doc": true}}
	rt.handle(context.Background(), newJob(1), func(context.Context, postgres.Job) error {
		return WithDoc("gone-doc", errors.New("insert chunk placement: violates foreign key"))
	})
	q.none(t)
}

func TestWorkForALiveDocumentKeepsItsOrdinaryRoute(t *testing.T) {
	q := &fakeQueue{}
	rt := newRuntime(q, 1)
	rt.Docs = &fakeDocs{missing: map[string]bool{}}
	rt.handle(context.Background(), newJob(1), func(context.Context, postgres.Job) error {
		return WithDoc("live-doc", errors.New("dependency unavailable"))
	})
	q.only(t, "retry")
}

// A verdict that cannot be recorded is logged and left for expiry. It is never
// retried blindly and never followed by a second, different transition.
func TestAFailedVerdictIsNotRetriedOrReplaced(t *testing.T) {
	q := &fakeQueue{retryErr: errors.New("connection lost")}
	rt := newRuntime(q, 1)
	rt.handle(context.Background(), newJob(1), func(context.Context, postgres.Job) error { return errors.New("boom") })
	q.only(t, "retry")
}

func TestAmbiguousFailureVerdictIsConfirmedFromDurableState(t *testing.T) {
	job := newJob(postgres.MaxAttempts)
	q := &fakeQueue{failErr: errors.New("timeout after commit"), state: func(postgres.Job) postgres.JobState {
		return postgres.JobState{Exists: true, State: "FAILED", Generation: job.Generation}
	}}
	rt := newRuntime(q, 1)
	reason := string(domain.ReasonUnclassified)
	before := testutil.ToFloat64(telemetry.DLQ.WithLabelValues("test", reason))
	rt.handle(context.Background(), job, func(context.Context, postgres.Job) error { return errors.New("boom") })
	if got := testutil.ToFloat64(telemetry.DLQ.WithLabelValues("test", reason)); got != before+1 {
		t.Errorf("a committed failure was counted %v times, want once", got-before)
	}

	// Not committed: it must not be counted.
	q2 := &fakeQueue{failErr: errors.New("timeout before commit"), state: func(postgres.Job) postgres.JobState {
		return postgres.JobState{Exists: true, State: "RUNNING", Generation: job.Generation}
	}}
	rt2 := newRuntime(q2, 1)
	before = testutil.ToFloat64(telemetry.DLQ.WithLabelValues("test", reason))
	rt2.handle(context.Background(), job, func(context.Context, postgres.Job) error { return errors.New("boom") })
	if got := testutil.ToFloat64(telemetry.DLQ.WithLabelValues("test", reason)); got != before {
		t.Errorf("an unrecorded failure was counted")
	}
}

func TestUncertainCommitIsResolvedFromDurableState(t *testing.T) {
	uncertain := func(postgres.Job) error { return fmt.Errorf("%w: connection reset", postgres.ErrCommitUncertain) }
	completed := testutil.ToFloat64(telemetry.IngestionTasks.WithLabelValues("test", "completed"))

	// It committed: counted as completed, nothing else recorded.
	job := newJob(1)
	q := &fakeQueue{runErr: uncertain, state: func(postgres.Job) postgres.JobState {
		return postgres.JobState{Exists: true, State: "COMPLETED", Generation: job.Generation}
	}}
	newRuntime(q, 1).handle(context.Background(), job, func(context.Context, postgres.Job) error { return nil })
	q.none(t)
	if got := testutil.ToFloat64(telemetry.IngestionTasks.WithLabelValues("test", "completed")); got != completed+1 {
		t.Errorf("a resolved commit was counted %v times", got-completed)
	}

	// It did not commit and the claim is still ours: an ordinary retry.
	q = &fakeQueue{runErr: uncertain, state: func(postgres.Job) postgres.JobState {
		return postgres.JobState{Exists: true, State: "RUNNING", Generation: job.Generation}
	}}
	newRuntime(q, 1).handle(context.Background(), job, func(context.Context, postgres.Job) error { return nil })
	q.only(t, "retry")

	// The job moved on: no verdict.
	q = &fakeQueue{runErr: uncertain, state: func(postgres.Job) postgres.JobState {
		return postgres.JobState{Exists: true, State: "RUNNING", Generation: job.Generation + 1}
	}}
	newRuntime(q, 1).handle(context.Background(), job, func(context.Context, postgres.Job) error { return nil })
	q.none(t)
}

// --- heartbeat -------------------------------------------------------------

func TestHeartbeatRenewsUntilStopped(t *testing.T) {
	shortTimings(t)
	q := &fakeQueue{}
	rt := newRuntime(q, 1)
	_, cancel := context.WithCancelCause(context.Background())
	stop := rt.startHeartbeat(newJob(1), cancel)
	waitFor(t, "several renewals", func() bool { return q.renewals.Load() >= 3 })
	stop()
	settled := q.renewals.Load()
	time.Sleep(50 * time.Millisecond)
	if got := q.renewals.Load(); got != settled {
		t.Errorf("renewed %d more times after stop; a finished handler must stop renewing", got-settled)
	}
	stop() // idempotent: handle() stops on a path a panic's recovery can also reach
}

func TestHeartbeatZeroRowsCancelsTheHandlerAtOnce(t *testing.T) {
	shortTimings(t)
	q := &fakeQueue{renew: func(context.Context, postgres.Job) (postgres.Lease, error) {
		return postgres.Lease{}, postgres.ErrLeaseLost
	}}
	rt := newRuntime(q, 1)
	ctx, cancel := context.WithCancelCause(context.Background())
	stop := rt.startHeartbeat(newJob(1), cancel)
	defer stop()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("confirmed lease loss did not cancel the handler")
	}
	if !errors.Is(context.Cause(ctx), postgres.ErrLeaseLost) {
		t.Errorf("cause = %v, want ErrLeaseLost", context.Cause(ctx))
	}
}

// One transient failure must not cancel healthy work.
func TestHeartbeatSurvivesABriefOutage(t *testing.T) {
	shortTimings(t)
	renewMargin = time.Millisecond
	var calls atomic.Int64
	q := &fakeQueue{renew: func(context.Context, postgres.Job) (postgres.Lease, error) {
		if calls.Add(1) <= 3 {
			return postgres.Lease{}, errors.New("connection lost")
		}
		now := time.Now()
		return postgres.Lease{Until: now.Add(time.Minute), ServerNow: now}, nil
	}}
	rt := newRuntime(q, 1)
	ctx, cancel := context.WithCancelCause(context.Background())
	stop := rt.startHeartbeat(newJob(1), cancel)
	defer stop()
	waitFor(t, "renewals to resume after the outage", func() bool { return calls.Load() >= 6 })
	if ctx.Err() != nil {
		t.Fatalf("a brief outage cancelled the handler: %v", context.Cause(ctx))
	}
}

// When no renewal can be confirmed before the conservative local expiry minus
// the safety margin, the handler is cancelled and its claim left to expire.
func TestHeartbeatCancelsWhenUnconfirmedInsideTheSafetyWindow(t *testing.T) {
	shortTimings(t)
	q := &fakeQueue{renew: func(context.Context, postgres.Job) (postgres.Lease, error) {
		return postgres.Lease{}, errors.New("database unreachable")
	}}
	rt := newRuntime(q, 1)
	job := newJob(1)
	job.LeaseUntil = job.ServerNow.Add(120 * time.Millisecond) // short lease, local estimate follows
	ctx, cancel := context.WithCancelCause(context.Background())
	stop := rt.startHeartbeat(job, cancel)
	defer stop()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("an unconfirmable lease never cancelled the handler")
	}
	if !errors.Is(context.Cause(ctx), errLeaseUncertain) {
		t.Errorf("cause = %v, want the uncertain-lease cause", context.Cause(ctx))
	}
}

// The local estimate is request-start plus the remaining time the database
// reported, so response delay can only make it earlier: the handler is
// cancelled well before the database's own expiry, never after it.
func TestHeartbeatEstimateNeverExceedsTheDatabaseLease(t *testing.T) {
	shortTimings(t)
	renewMargin = 50 * time.Millisecond
	renewEvery = 20 * time.Millisecond
	var granted atomic.Int64 // unix nanos of the lease end the database reported
	q := &fakeQueue{renew: func(context.Context, postgres.Job) (postgres.Lease, error) {
		time.Sleep(15 * time.Millisecond) // a slow response
		now := time.Now()
		until := now.Add(60 * time.Millisecond)
		granted.Store(until.UnixNano())
		return postgres.Lease{Until: until, ServerNow: now}, nil
	}}
	rt := newRuntime(q, 1)
	job := newJob(1)
	job.LeaseUntil = job.ServerNow.Add(150 * time.Millisecond)
	ctx, cancel := context.WithCancelCause(context.Background())
	stop := rt.startHeartbeat(job, cancel)
	defer stop()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the handler outlived its lease estimate")
	}
	cancelledAt := time.Now()
	if q.renewals.Load() == 0 {
		t.Fatal("the heartbeat never attempted a renewal before cancelling")
	}
	if end := time.Unix(0, granted.Load()); !cancelledAt.Before(end) {
		t.Errorf("handler cancelled at %v, not before the database's lease end %v", cancelledAt, end)
	}
}

// The handler's deadline runs from claim time, not from when the lane happened
// to pick the job up.
func TestHandlerDeadlineRunsFromClaimTime(t *testing.T) {
	shortTimings(t)
	q := &fakeQueue{}
	rt := newRuntime(q, 1)
	job := newJob(1)
	job.Started = time.Now().Add(-postgres.HandlerBudget + 60*time.Millisecond)
	var deadline time.Time
	rt.handle(context.Background(), job, func(ctx context.Context, _ postgres.Job) error {
		deadline, _ = ctx.Deadline()
		<-ctx.Done()
		return ctx.Err()
	})
	if deadline.IsZero() || time.Until(deadline) > time.Second {
		t.Fatalf("handler deadline %v is not anchored to the claim", deadline)
	}
}

// --- backoff and budgets ---------------------------------------------------

// Retrying into a breaker that is still open guarantees another refusal and
// spends one of only three attempts to learn nothing.
func TestBackoffOutlastsTheBreakerCooldown(t *testing.T) {
	const breakerCooldown = 30 * time.Second // embedding.NewBreaker(5, 30s)
	for attempts := 1; attempts <= postgres.MaxAttempts; attempts++ {
		for i := 0; i < 200; i++ {
			if got := nakBackoff(attempts); got < breakerCooldown {
				t.Fatalf("attempt %d waits %v, inside the %v cooldown", attempts, got, breakerCooldown)
			}
		}
	}
}

func TestBackoffGrowsThenStaysBounded(t *testing.T) {
	// Jitter bands for successive attempts do not overlap, so samples can be
	// compared directly rather than statistically.
	if first, second := nakBackoff(1), nakBackoff(2); second <= first {
		t.Errorf("attempt 2 waited %v, no longer than attempt 1's %v", second, first)
	}
	for _, attempts := range []int{4, 8, 64, 1 << 20} {
		got := nakBackoff(attempts)
		if got > nakBackoffMax {
			t.Errorf("attempt %d waited %v, past the %v ceiling", attempts, got, nakBackoffMax)
		}
		if got <= 0 {
			t.Errorf("attempt %d waited %v; a large count must saturate, not overflow", attempts, got)
		}
	}
}

// 49,000 jobs failing together must not come back together and re-break what
// they were waiting for.
func TestBackoffIsJittered(t *testing.T) {
	seen := make(map[time.Duration]bool)
	for i := 0; i < 100; i++ {
		seen[nakBackoff(1)] = true
	}
	if len(seen) < 10 {
		t.Errorf("only %d distinct delays in 100 draws; a batch that failed together would return together", len(seen))
	}
}

// The budget has to clear the slowest work that legitimately exists, or it
// re-creates the cascade the heartbeat was introduced to stop.
func TestHandlerBudgetClearsTheSlowestKnownDocument(t *testing.T) {
	const slowestKnownWork = 15 * time.Minute // 208-page scanned PDF, ~4.5s/page
	if postgres.HandlerBudget <= slowestKnownWork {
		t.Fatalf("budget %v does not clear the %v a real document needs", postgres.HandlerBudget, slowestKnownWork)
	}
	if postgres.HandlerBudget > 6*time.Hour {
		t.Errorf("budget %v is so large it is unbounded in practice", postgres.HandlerBudget)
	}
	if postgres.LeaseDuration <= renewEvery*4 {
		t.Errorf("lease %v leaves too little margin for a %v renewal cadence", postgres.LeaseDuration, renewEvery)
	}
}
