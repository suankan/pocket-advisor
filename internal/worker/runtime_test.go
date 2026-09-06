package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/suankan/pocket-advisor/internal/bus"
	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/telemetry"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// The pool is the component the whole refactor rests on: it replaced a serial
// loop whose real parallelism was 1, so "does it actually run N at once, and
// does it stay at N" is the property worth pinning down.
func TestConsumeRunsLanesConcurrentlyAndRespectsTheBound(t *testing.T) {
	const (
		lanes    = 8
		messages = 60
	)
	consumer := newFakeConsumer(messages)
	rt := &Runtime{
		Name: "test", Subject: "test.subject", Log: quietLogger(),
		Lanes: lanes, Bus: &fakeDLQ{},
	}

	var mu sync.Mutex
	concurrent, peak := 0, 0
	var handled atomic.Int64

	fetchCtx, stopFetch := context.WithCancel(context.Background())
	defer stopFetch()

	go func() {
		for handled.Load() < messages {
			time.Sleep(time.Millisecond)
		}
		stopFetch()
	}()

	err := rt.Consume(fetchCtx, context.Background(), consumer,
		func(context.Context, jetstream.Msg) error {
			mu.Lock()
			concurrent++
			if concurrent > peak {
				peak = concurrent
			}
			mu.Unlock()

			time.Sleep(3 * time.Millisecond)

			mu.Lock()
			concurrent--
			mu.Unlock()
			handled.Add(1)
			return nil
		})
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}

	if peak > lanes {
		t.Errorf("peak concurrency %d exceeded Lanes=%d", peak, lanes)
	}
	if peak < 2 {
		t.Errorf("peak concurrency %d — the pool is still serialising", peak)
	}
	if got := consumer.acked.Load(); got == 0 {
		t.Error("no messages were acked")
	}
}

// A message must never be fetched into an in-process buffer it can sit in,
// because the broker starts its AckWait the moment it hands the message over.
func TestConsumeNeverHoldsMoreThanLanesInFlight(t *testing.T) {
	const lanes = 4
	consumer := newFakeConsumer(40)

	rt := &Runtime{
		Name: "test", Subject: "test.subject", Log: quietLogger(),
		Lanes: lanes, Bus: &fakeDLQ{},
	}

	var handed atomic.Int64
	consumer.onFetch = func(batch int) {
		if batch > lanes {
			t.Errorf("fetched %d messages for %d lanes", batch, lanes)
		}
	}

	fetchCtx, stopFetch := context.WithCancel(context.Background())
	go func() {
		for handed.Load() < 30 {
			time.Sleep(time.Millisecond)
		}
		stopFetch()
	}()

	_ = rt.Consume(fetchCtx, context.Background(), consumer,
		func(context.Context, jetstream.Msg) error {
			handed.Add(1)
			time.Sleep(2 * time.Millisecond)
			return nil
		})
}

// A panic used to kill one pod and be restarted by Kubernetes. In one process
// it would take every role down, so it has to become a terminal failure for
// that message alone while the pool keeps running.
func TestConsumeRecoversFromHandlerPanic(t *testing.T) {
	consumer := newFakeConsumer(6)
	dlq := &fakeDLQ{}
	rt := &Runtime{
		Name: "test", Subject: "test.subject", Log: quietLogger(),
		Lanes: 2, Bus: dlq,
	}

	var seen atomic.Int64
	fetchCtx, stopFetch := context.WithCancel(context.Background())
	go func() {
		for seen.Load() < 6 {
			time.Sleep(time.Millisecond)
		}
		stopFetch()
	}()

	err := rt.Consume(fetchCtx, context.Background(), consumer,
		func(context.Context, jetstream.Msg) error {
			n := seen.Add(1)
			if n%2 == 0 {
				panic("handler exploded")
			}
			return nil
		})
	if err != nil {
		t.Fatalf("Consume returned %v — a panic escaped the pool", err)
	}

	if dlq.count.Load() == 0 {
		t.Error("panicking messages were not routed to the DLQ")
	}
	if consumer.termed.Load() == 0 {
		t.Error("panicking messages were not terminated")
	}
	if consumer.acked.Load() == 0 {
		t.Error("the pool stopped processing good messages after a panic")
	}
}

// The first Ctrl+C stops fetching but must let in-flight work finish and ack.
// Abandoning it unacked would burn one of its three delivery attempts, and
// three interrupted runs would dead-letter a perfectly good document.
func TestConsumeDrainsInFlightWorkAfterFetchStops(t *testing.T) {
	consumer := newFakeConsumer(4)
	rt := &Runtime{
		Name: "test", Subject: "test.subject", Log: quietLogger(),
		Lanes: 2, Bus: &fakeDLQ{},
	}

	started := make(chan struct{}, 4)
	var completed atomic.Int64

	fetchCtx, stopFetch := context.WithCancel(context.Background())

	go func() {
		<-started
		// Stop fetching while a handler is provably mid-flight.
		stopFetch()
	}()

	err := rt.Consume(fetchCtx, context.Background(), consumer,
		func(context.Context, jetstream.Msg) error {
			select {
			case started <- struct{}{}:
			default:
			}
			time.Sleep(50 * time.Millisecond)
			completed.Add(1)
			return nil
		})
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}

	if completed.Load() == 0 {
		t.Fatal("in-flight work was abandoned instead of drained")
	}
	if consumer.acked.Load() < completed.Load() {
		t.Errorf("acked %d but completed %d — drained work was not acked",
			consumer.acked.Load(), completed.Load())
	}
}

func TestConsumeCountsOutcomesForTheDashboard(t *testing.T) {
	consumer := newFakeConsumer(4)
	stats := telemetry.NewStats()
	q := stats.RegisterQueue("test", "test.subject", 2)

	rt := &Runtime{
		Name: "test", Subject: "test.subject", Log: quietLogger(),
		Lanes: 2, Bus: &fakeDLQ{}, Stats: q,
	}

	var seen atomic.Int64
	fetchCtx, stopFetch := context.WithCancel(context.Background())
	go func() {
		for seen.Load() < 4 {
			time.Sleep(time.Millisecond)
		}
		stopFetch()
	}()

	_ = rt.Consume(fetchCtx, context.Background(), consumer,
		func(context.Context, jetstream.Msg) error {
			if seen.Add(1)%2 == 0 {
				return Decline("doc-1", "UNSUPPORTED")
			}
			return nil
		})

	snap := q.Snapshot()
	if snap.Completed == 0 {
		t.Error("no completions recorded")
	}
	if snap.Skipped == 0 {
		t.Error("declines were not recorded as skips")
	}
	if snap.Active != 0 {
		t.Errorf("Active = %d after drain, want 0", snap.Active)
	}
}

// --- fakes -----------------------------------------------------------------

type fakeDLQ struct{ count atomic.Int64 }

func (f *fakeDLQ) ToDLQ(context.Context, string, string, string, string, uint64, []byte) error {
	f.count.Add(1)
	return nil
}

type fakeMsg struct {
	c *fakeConsumer
}

func (m *fakeMsg) Data() []byte         { return []byte("payload") }
func (m *fakeMsg) Subject() string      { return "test.subject" }
func (m *fakeMsg) Reply() string        { return "" }
func (m *fakeMsg) Headers() nats.Header { return nats.Header{} }
func (m *fakeMsg) Metadata() (*jetstream.MsgMetadata, error) {
	n := m.c.deliveries.Load()
	if n == 0 {
		n = 1
	}
	return &jetstream.MsgMetadata{NumDelivered: uint64(n)}, nil
}
func (m *fakeMsg) Ack() error                      { m.c.acked.Add(1); return nil }
func (m *fakeMsg) DoubleAck(context.Context) error { m.c.acked.Add(1); return nil }
func (m *fakeMsg) Nak() error                      { m.c.naked.Add(1); return nil }

// Counted apart from Nak: whether a redelivery was asked for immediately or
// after a delay is the difference between pacing a failing dependency and
// spinning against it.
func (m *fakeMsg) NakWithDelay(d time.Duration) error {
	m.c.naked.Add(1)
	m.c.nakedWithDelay.Add(1)
	m.c.lastNakDelay.Store(int64(d))
	return nil
}
func (m *fakeMsg) InProgress() error           { m.c.inProgress.Add(1); return nil }
func (m *fakeMsg) Term() error                 { m.c.termed.Add(1); return nil }
func (m *fakeMsg) TermWithReason(string) error { m.c.termed.Add(1); return nil }

type fakeBatch struct{ ch chan jetstream.Msg }

func (b *fakeBatch) Messages() <-chan jetstream.Msg { return b.ch }
func (b *fakeBatch) Error() error                   { return nil }

// fakeConsumer serves a bounded supply of messages, then empty batches — the
// shape a real pull consumer presents once its queue drains.
type fakeConsumer struct {
	// deliveries is what Metadata reports as NumDelivered; 0 means "first".
	deliveries     atomic.Int64
	remaining      atomic.Int64
	acked          atomic.Int64
	naked          atomic.Int64
	nakedWithDelay atomic.Int64
	lastNakDelay   atomic.Int64
	termed         atomic.Int64
	inProgress     atomic.Int64
	onFetch        func(batch int)
}

func newFakeConsumer(n int) *fakeConsumer {
	c := &fakeConsumer{}
	c.remaining.Store(int64(n))
	return c
}

func (c *fakeConsumer) Fetch(batch int, _ ...jetstream.FetchOpt) (jetstream.MessageBatch, error) {
	if c.onFetch != nil {
		c.onFetch(batch)
	}
	ch := make(chan jetstream.Msg, batch)
	for i := 0; i < batch; i++ {
		if c.remaining.Add(-1) < 0 {
			c.remaining.Add(1)
			break
		}
		ch <- &fakeMsg{c: c}
	}
	close(ch)
	return &fakeBatch{ch: ch}, nil
}

func (c *fakeConsumer) FetchBytes(int, ...jetstream.FetchOpt) (jetstream.MessageBatch, error) {
	return nil, errors.New("not implemented")
}
func (c *fakeConsumer) FetchNoWait(batch int) (jetstream.MessageBatch, error) {
	return c.Fetch(batch)
}
func (c *fakeConsumer) Consume(jetstream.MessageHandler, ...jetstream.PullConsumeOpt) (jetstream.ConsumeContext, error) {
	return nil, errors.New("not implemented")
}
func (c *fakeConsumer) Messages(...jetstream.PullMessagesOpt) (jetstream.MessagesContext, error) {
	return nil, errors.New("not implemented")
}
func (c *fakeConsumer) Next(...jetstream.FetchOpt) (jetstream.Msg, error) {
	return nil, errors.New("not implemented")
}
func (c *fakeConsumer) Info(context.Context) (*jetstream.ConsumerInfo, error) {
	return &jetstream.ConsumerInfo{NumPending: uint64(max64(c.remaining.Load(), 0))}, nil
}
func (c *fakeConsumer) CachedInfo() *jetstream.ConsumerInfo { return &jetstream.ConsumerInfo{} }

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// The heartbeat is what separates "this worker is slow" from "this worker is
// dead". Before it existed, a 208-page scanned PDF — ~15 minutes of OCR — was
// redelivered mid-flight and eventually dead-lettered as a terminal failure,
// with nothing wrong with the document.

func TestHeartbeatExtendsUntilStopped(t *testing.T) {
	c := newFakeConsumer(0)
	r := &Runtime{Name: "test", Log: quietLogger()}

	stop := r.heartbeatEvery(&fakeMsg{c: c}, time.Millisecond)

	// Wait for several ticks rather than sleeping a fixed span, so a slow
	// machine makes the test slower rather than flaky.
	deadline := time.Now().Add(2 * time.Second)
	for c.inProgress.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := c.inProgress.Load(); got < 3 {
		t.Fatalf("InProgress called %d times, want the deadline extended repeatedly", got)
	}

	stop()
	// A tick already selected when stop() ran still completes, so sample after
	// letting one land. The claim under test is that renewal ceases, not that
	// it ceases between two instructions.
	time.Sleep(20 * time.Millisecond)
	settled := c.inProgress.Load()
	time.Sleep(50 * time.Millisecond)
	if got := c.inProgress.Load(); got != settled {
		t.Errorf("InProgress called %d more times after stop; a finished handler must release the message", got-settled)
	}
}

func TestHeartbeatStopIsIdempotent(t *testing.T) {
	// handle() calls stop on a path that may also be reached by a panicking
	// handler's recovery, and closing a closed channel panics.
	c := newFakeConsumer(0)
	r := &Runtime{Name: "test", Log: quietLogger()}
	stop := r.heartbeatEvery(&fakeMsg{c: c}, time.Hour)
	stop()
	stop()
}

func TestHeartbeatIntervalLeavesMarginInsideAckWait(t *testing.T) {
	got := heartbeatInterval()
	if got <= 0 || got >= bus.AckWait {
		t.Fatalf("interval %v must be positive and inside AckWait %v", got, bus.AckWait)
	}
	// Several ticks per window, so one scheduled late under load is survivable.
	if min := bus.AckWait / 3; got > min {
		t.Errorf("interval %v leaves too little margin in a %v window (want <= %v)", got, bus.AckWait, min)
	}
}

// A failed InProgress must not disable the heartbeat. It is a network call, and
// one blip on a document that needs twenty minutes would silently restore the
// redelivery cascade the heartbeat exists to prevent.
func TestHeartbeatKeepsGoingAfterAFailedExtension(t *testing.T) {
	m := &flakyMsg{failUntil: 3}
	r := &Runtime{Name: "test", Log: quietLogger()}
	stop := r.heartbeatEvery(m, time.Millisecond)
	defer stop()

	deadline := time.Now().Add(2 * time.Second)
	for m.succeeded.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := m.succeeded.Load(); got < 2 {
		t.Fatalf("after %d failures the heartbeat stopped; %d later attempts succeeded, want it to keep trying",
			m.failUntil, got)
	}
}

// The other half of the heartbeat's job is knowing when to stop. Renewing
// without end answers "is this worker alive?" with "yes" on behalf of a
// handler that will never finish: eighteen wedged embed handlers once held
// eighteen messages open against a 49,000-message backlog, and because the
// broker was told the work was in hand it redelivered none of them. Only
// killing the process — the one thing that stops a heartbeat — cleared it.
func TestHeartbeatStopsRenewingAtItsBudget(t *testing.T) {
	c := newFakeConsumer(0)
	r := &Runtime{Name: "test", Log: quietLogger()}

	stop := r.heartbeatFor(&fakeMsg{c: c}, time.Millisecond, 25*time.Millisecond)
	defer stop()

	// Well past the budget, then a second look: the count must have frozen.
	time.Sleep(250 * time.Millisecond)
	settled := c.inProgress.Load()
	time.Sleep(100 * time.Millisecond)

	if settled == 0 {
		t.Fatal("heartbeat never renewed; the budget must bound renewal, not disable it")
	}
	if got := c.inProgress.Load(); got != settled {
		t.Errorf("heartbeat renewed %d more times after its budget elapsed; a wedged handler would hold its message for the life of the process",
			got-settled)
	}
}

// A budget of zero is the unbounded form, which the tests above rely on.
func TestHeartbeatWithoutABudgetRenewsIndefinitely(t *testing.T) {
	c := newFakeConsumer(0)
	r := &Runtime{Name: "test", Log: quietLogger()}

	stop := r.heartbeatFor(&fakeMsg{c: c}, time.Millisecond, 0)
	defer stop()

	deadline := time.Now().Add(2 * time.Second)
	for c.inProgress.Load() < 5 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := c.inProgress.Load(); got < 5 {
		t.Errorf("InProgress called %d times, want an unbudgeted heartbeat to keep renewing", got)
	}
}

// The budget has to clear the slowest work that legitimately exists, or it
// re-creates the very cascade the heartbeat was introduced to stop.
func TestHandlerBudgetClearsTheSlowestKnownDocument(t *testing.T) {
	const slowestKnownWork = 15 * time.Minute // 208-page scanned PDF, ~4.5s/page

	if maxHandlerRuntime <= slowestKnownWork {
		t.Fatalf("budget %v does not clear the %v a real document needs; healthy work would be abandoned mid-flight",
			maxHandlerRuntime, slowestKnownWork)
	}
	if maxHandlerRuntime > 6*time.Hour {
		t.Errorf("budget %v is so large it is unbounded in practice", maxHandlerRuntime)
	}
}

// Redelivering into a breaker that is still open guarantees another refusal
// and spends one of only three delivery attempts to learn nothing.
func TestNakBackoffOutlastsTheBreakerCooldown(t *testing.T) {
	const breakerCooldown = 30 * time.Second // embedding.NewBreaker(5, 30s)

	for deliveries := uint64(1); deliveries <= bus.MaxDeliver; deliveries++ {
		for i := 0; i < 200; i++ {
			if got := nakBackoff(deliveries); got < breakerCooldown {
				t.Fatalf("delivery %d waits %v, inside the %v cooldown: the retry is certain to be refused",
					deliveries, got, breakerCooldown)
			}
		}
	}
}

func TestNakBackoffGrowsThenStaysBounded(t *testing.T) {
	// Jitter bands for successive deliveries do not overlap, so samples can be
	// compared directly rather than statistically.
	if first, second := nakBackoff(1), nakBackoff(2); second <= first {
		t.Errorf("delivery 2 waited %v, no longer than delivery 1's %v", second, first)
	}
	for _, deliveries := range []uint64{4, 8, 64, 1 << 20} {
		got := nakBackoff(deliveries)
		if got > nakBackoffMax {
			t.Errorf("delivery %d waited %v, past the %v ceiling", deliveries, got, nakBackoffMax)
		}
		if got <= 0 {
			t.Errorf("delivery %d waited %v; a large count must saturate, not overflow", deliveries, got)
		}
	}
}

// 49,000 messages failing together must not come back together and re-break
// what they were waiting for.
func TestNakBackoffIsJittered(t *testing.T) {
	seen := make(map[time.Duration]bool)
	for i := 0; i < 100; i++ {
		seen[nakBackoff(1)] = true
	}
	if len(seen) < 10 {
		t.Errorf("only %d distinct delays in 100 draws; a batch that failed together would return together", len(seen))
	}
}

// The pacing is worth nothing if the retry path does not actually use it.
func TestTransientFailureIsRedeliveredWithADelay(t *testing.T) {
	consumer := newFakeConsumer(1)
	rt := &Runtime{
		Name: "test", Subject: "test.subject", Log: quietLogger(),
		Lanes: 1, Bus: &fakeDLQ{},
	}

	fetchCtx, stopFetch := context.WithCancel(context.Background())
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for consumer.naked.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		stopFetch()
	}()

	_ = rt.Consume(fetchCtx, context.Background(), consumer,
		func(context.Context, jetstream.Msg) error {
			return errors.New("dependency unavailable")
		})

	if consumer.nakedWithDelay.Load() == 0 {
		t.Fatal("a transient failure asked for immediate redelivery; under a failing dependency that spends all three attempts in milliseconds")
	}
	if got := time.Duration(consumer.lastNakDelay.Load()); got <= 0 {
		t.Errorf("redelivery delay was %v, want a positive backoff", got)
	}
}

// flakyMsg fails the first failUntil calls to InProgress, then succeeds.
type flakyMsg struct {
	fakeMsg
	failUntil int64
	attempts  atomic.Int64
	succeeded atomic.Int64
}

func (m *flakyMsg) InProgress() error {
	if m.attempts.Add(1) <= m.failUntil {
		return errors.New("connection lost")
	}
	m.succeeded.Add(1)
	return nil
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

// Pruning and the queues are not transactional with each other, so work
// enqueued before a sweep can name a document the sweep has since deleted.
// That is not a failure of the work and must not reach the DLQ: an operator
// cannot go and inspect a document that was deliberately removed.
func TestWorkForAPrunedDocumentIsDiscardedNotDeadLettered(t *testing.T) {
	consumer := newFakeConsumer(1)
	dlq := &fakeDLQ{}
	docs := &fakeDocs{missing: map[string]bool{"gone-doc": true}}
	rt := &Runtime{
		Name: "test", Subject: "test.subject", Log: quietLogger(),
		Lanes: 1, Bus: dlq, Docs: docs,
	}

	fetchCtx, stopFetch := context.WithCancel(context.Background())
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for consumer.acked.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		stopFetch()
	}()

	_ = rt.Consume(fetchCtx, context.Background(), consumer,
		func(context.Context, jetstream.Msg) error {
			// What a worker actually reports once its document is gone: the
			// write it attempted had nothing left to reference.
			return WithDoc("gone-doc", errors.New("insert chunk placement: violates foreign key"))
		})

	if consumer.acked.Load() == 0 {
		t.Error("work for a pruned document was not acked; it will be redelivered forever")
	}
	if dlq.count.Load() != 0 {
		t.Errorf("dead-lettered %d message(s) for a document that no longer exists", dlq.count.Load())
	}
	if consumer.termed.Load() != 0 {
		t.Errorf("terminated %d message(s) instead of discarding", consumer.termed.Load())
	}
}

// The same failure against a document that still exists must keep its ordinary
// route, or the discard would swallow real breakage.
func TestWorkForALiveDocumentStillFails(t *testing.T) {
	consumer := newFakeConsumer(1)
	dlq := &fakeDLQ{}
	docs := &fakeDocs{missing: map[string]bool{}} // everything still present
	rt := &Runtime{
		Name: "test", Subject: "test.subject", Log: quietLogger(),
		Lanes: 1, Bus: dlq, Docs: docs,
	}

	fetchCtx, stopFetch := context.WithCancel(context.Background())
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for consumer.naked.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		stopFetch()
	}()

	_ = rt.Consume(fetchCtx, context.Background(), consumer,
		func(context.Context, jetstream.Msg) error {
			return WithDoc("live-doc", errors.New("dependency unavailable"))
		})

	if consumer.naked.Load() == 0 {
		t.Error("a transient failure on a live document was not retried")
	}
	if consumer.acked.Load() != 0 {
		t.Error("a live document's failure was discarded as if it had been pruned")
	}
}

// Shutdown bypasses the application's failure threshold. The real-broker
// regression additionally proves that this NAK remains deliverable.
func TestShutdownCancellationIsNotADeadLetter(t *testing.T) {
	consumer := newFakeConsumer(1)
	// The third interrupted run: two attempts already spent, so without the
	// guard this delivery is the one that dead-letters.
	consumer.deliveries.Store(int64(bus.MaxDeliver))
	dlq := &fakeDLQ{}
	rt := &Runtime{
		Name: "test", Subject: "test.subject", Log: quietLogger(),
		Lanes: 1, Bus: dlq,
	}

	// A work context that is already cancelled: what a handler sees once the
	// run is being torn down around it.
	workCtx, cancelWork := context.WithCancel(context.Background())
	cancelWork()

	fetchCtx, stopFetch := context.WithCancel(context.Background())
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for consumer.naked.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		stopFetch()
	}()

	_ = rt.Consume(fetchCtx, workCtx, consumer,
		func(context.Context, jetstream.Msg) error {
			// What a store reports when everything under it is cancelled.
			return errors.New(`stat "raw/69/6934": context canceled`)
		})

	if dlq.count.Load() != 0 {
		t.Errorf("dead-lettered %d message(s) during shutdown; three such runs lose a good document",
			dlq.count.Load())
	}
	if consumer.termed.Load() != 0 {
		t.Errorf("terminated %d message(s) during shutdown", consumer.termed.Load())
	}
	if consumer.naked.Load() == 0 {
		t.Error("the message was not handed back for the next run")
	}
}
