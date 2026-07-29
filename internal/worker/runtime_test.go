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
	return &jetstream.MsgMetadata{NumDelivered: 1}, nil
}
func (m *fakeMsg) Ack() error                       { m.c.acked.Add(1); return nil }
func (m *fakeMsg) DoubleAck(context.Context) error  { m.c.acked.Add(1); return nil }
func (m *fakeMsg) Nak() error                       { m.c.naked.Add(1); return nil }
func (m *fakeMsg) NakWithDelay(time.Duration) error { m.c.naked.Add(1); return nil }
func (m *fakeMsg) InProgress() error                { return nil }
func (m *fakeMsg) Term() error                      { m.c.termed.Add(1); return nil }
func (m *fakeMsg) TermWithReason(string) error      { m.c.termed.Add(1); return nil }

type fakeBatch struct{ ch chan jetstream.Msg }

func (b *fakeBatch) Messages() <-chan jetstream.Msg { return b.ch }
func (b *fakeBatch) Error() error                   { return nil }

// fakeConsumer serves a bounded supply of messages, then empty batches — the
// shape a real pull consumer presents once its queue drains.
type fakeConsumer struct {
	remaining atomic.Int64
	acked     atomic.Int64
	naked     atomic.Int64
	termed    atomic.Int64
	onFetch   func(batch int)
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
