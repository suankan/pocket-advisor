// Package worker is the transport layer: the only place that knows about
// JetStream. It fetches, unmarshals, calls an engine, and dispatches
// Ack/Nak/Term (ingestion-design.md §8.2).
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/suankan/pocket-advisor/internal/bus"
	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/telemetry"
)

// Terminal marks an error as not worth retrying: the work is broken in a way
// a redelivery cannot fix, so it goes straight to the DLQ.
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
// failure: the document is recorded SKIPPED and the message is acked. Mixing
// "we can't parse this" with "this broke" makes the DLQ unactionable (§2.5).
type Declined struct {
	Reason domain.FailureReason
	DocID  string
}

func (d *Declined) Error() string { return "declined: " + string(d.Reason) }

func Decline(docID string, reason domain.FailureReason) error {
	return &Declined{Reason: reason, DocID: docID}
}

// Handler processes one message. Returning nil acks it.
type Handler func(ctx context.Context, msg jetstream.Msg) error

// DeadLetterer is the half of the DLQ protocol the runtime needs. Narrowed to
// an interface so the pool's failure paths are testable without a broker;
// *bus.Bus is the only production implementation.
type DeadLetterer interface {
	ToDLQ(ctx context.Context, origSubject, worker, reason, traceparent string, deliveries uint64, payload []byte) error
}

type Runtime struct {
	Name    string
	Bus     DeadLetterer
	Docs    *postgres.DocumentRepo
	Log     *slog.Logger
	Subject string

	// Lanes is how many messages this pool handles at once. Replaces the old
	// Batch, which only controlled how many messages were fetched per call —
	// they were then processed one at a time in the fetching goroutine, so a
	// pod's real parallelism was 1 and scaling meant adding replicas.
	Lanes int

	// Stats is the live counter set for this subject. Optional: nil disables
	// dashboard reporting without affecting behaviour.
	Stats *telemetry.Queue
}

// Consume runs a bounded worker pool until fetchCtx is done, then drains.
//
// The two contexts have deliberately different lifetimes. fetchCtx ending stops
// new work being pulled; workCtx bounds the handlers themselves and must
// outlive it. That gap is what makes Ctrl+C safe: in-flight documents finish
// and ack normally instead of being abandoned unacked, which would burn one of
// their three delivery attempts and eventually dead-letter perfectly good work.
func (r *Runtime) Consume(fetchCtx, workCtx context.Context, consumer jetstream.Consumer, h Handler) error {
	lanes := r.Lanes
	if lanes < 1 {
		lanes = 1
	}

	work := make(chan jetstream.Msg)
	freed := make(chan struct{}, 1)
	var inFlight atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < lanes; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for msg := range work {
				r.handle(workCtx, msg, h)
				inFlight.Add(-1)
				// Wake the fetcher if it is parked waiting for a free lane. A
				// buffered-one channel and a non-blocking send make this a
				// signal rather than a queue.
				select {
				case freed <- struct{}{}:
				default:
				}
			}
		}()
	}

	r.fetchLoop(fetchCtx, consumer, work, freed, &inFlight, lanes)

	// Closing work lets the lanes finish what they hold and exit. Nothing new
	// enters after this point.
	close(work)
	wg.Wait()
	return nil
}

// fetchLoop pulls only as many messages as there are idle lanes.
//
// Fetching more would park them in an in-process buffer where their AckWait
// keeps running while they wait — the broker considers a message delivered the
// moment it hands it over, so an over-eager fetch converts queue depth into
// redelivery risk for no throughput gain.
func (r *Runtime) fetchLoop(
	ctx context.Context,
	consumer jetstream.Consumer,
	work chan<- jetstream.Msg,
	freed <-chan struct{},
	inFlight *atomic.Int64,
	lanes int,
) {
	for {
		if ctx.Err() != nil {
			return
		}

		free := lanes - int(inFlight.Load())
		if free < 1 {
			select {
			case <-ctx.Done():
				return
			case <-freed:
			case <-time.After(time.Second):
			}
			continue
		}

		msgs, err := consumer.Fetch(free, jetstream.FetchMaxWait(2*time.Second))
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			r.Log.Warn("fetch failed", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}

		for msg := range msgs.Messages() {
			inFlight.Add(1)
			select {
			case work <- msg:
			case <-ctx.Done():
				// Hand it back rather than dropping it: an unacked Nak
				// redelivers immediately instead of after the ack window.
				inFlight.Add(-1)
				_ = msg.Nak()
			}
		}
		if err := msgs.Error(); err != nil && ctx.Err() == nil {
			r.Log.Warn("fetch batch error", "error", err)
		}
	}
}

// invoke calls the handler with a panic guard.
//
// One process now hosts every role. A panic that used to kill a single pod and
// be restarted by Kubernetes would take the whole pipeline down, so it is
// converted into a terminal failure for that one message instead.
func (r *Runtime) invoke(ctx context.Context, msg jetstream.Msg, h Handler) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = Fatal(domain.ReasonHandlerPanic, fmt.Errorf("%v\n%s", p, debug.Stack()))
		}
	}()
	return h(ctx, msg)
}

func (r *Runtime) handle(ctx context.Context, msg jetstream.Msg, h Handler) {
	if r.Stats != nil {
		r.Stats.LaneStarted()
		defer r.Stats.LaneFinished()
	}

	start := time.Now()
	err := r.invoke(ctx, msg, h)
	telemetry.IngestionDuration.WithLabelValues(r.Name, r.Subject).Observe(time.Since(start).Seconds())

	switch {
	case err == nil:
		telemetry.IngestionTasks.WithLabelValues(r.Name, "completed").Inc()
		r.stat((*telemetry.Queue).Completed)
		_ = msg.Ack()
		return

	case isDeclined(err):
		var d *Declined
		errors.As(err, &d)
		telemetry.IngestionTasks.WithLabelValues(r.Name, "skipped").Inc()
		telemetry.Skipped.WithLabelValues(string(d.Reason)).Inc()
		r.stat((*telemetry.Queue).Skipped)
		if d.DocID != "" && r.Docs != nil {
			if uerr := r.Docs.UpdateStatus(ctx, d.DocID, domain.StatusSkipped, d.Reason); uerr != nil {
				r.Log.Error("record skip", "doc_id", d.DocID, "error", uerr)
			}
		}
		r.Log.Info("declined", "reason", d.Reason, "doc_id", d.DocID)
		_ = msg.Ack()
		return
	}

	meta, _ := msg.Metadata()
	deliveries := uint64(0)
	if meta != nil {
		deliveries = meta.NumDelivered
	}

	terminal := isTerminal(err)
	if terminal || deliveries >= bus.MaxDeliver {
		// The worker performs the DLQ hop itself rather than relying on the
		// MaxDeliveries advisory, which is a backstop for crashed workers that
		// never reach their own error path (§2.5).
		reason := reasonOf(err)
		if derr := r.Bus.ToDLQ(ctx, r.Subject, r.Name, string(reason),
			msg.Headers().Get(bus.HdrTraceparent), deliveries, msg.Data()); derr != nil {
			r.Log.Error("dlq publish failed", "error", derr)
		}
		telemetry.IngestionTasks.WithLabelValues(r.Name, "dlq").Inc()
		telemetry.DLQ.WithLabelValues(r.Name, string(reason)).Inc()
		r.stat((*telemetry.Queue).DeadLettered)

		if id := docIDOf(err); id != "" && r.Docs != nil {
			_ = r.Docs.UpdateStatus(ctx, id, domain.StatusFailed, reason)
		}
		r.Log.Error("terminal failure, routed to dlq",
			"error", err, "deliveries", deliveries, "reason", reason)
		_ = msg.Term()
		return
	}

	telemetry.IngestionTasks.WithLabelValues(r.Name, "retry").Inc()
	r.stat((*telemetry.Queue).Retried)
	r.Log.Warn("transient failure, will redeliver",
		"error", err, "delivery", deliveries, "max", bus.MaxDeliver)
	_ = msg.Nak()
}

// stat applies a counter method when the dashboard is attached.
func (r *Runtime) stat(f func(*telemetry.Queue)) {
	if r.Stats != nil {
		f(r.Stats)
	}
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
	// Deliberately not ReasonExtractionFailed. Defaulting to a real class made
	// every unclassified error indistinguishable from a genuine extraction
	// failure, which is how a RustFS outage came to be recorded as 44 broken
	// documents. UNCLASSIFIED says the true thing: nobody named this path yet.
	return domain.ReasonUnclassified
}

// docIDErr lets a handler attach the document a failure belongs to, so the
// Tier 2 row can be marked FAILED alongside the DLQ hop.
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
