// Package worker is the transport layer: the only place that knows about
// JetStream. It fetches, unmarshals, calls an engine, and dispatches
// Ack/Nak/Term (ingestion-design.md §8.2).
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	Reason string
	Err    error
}

func (t *Terminal) Error() string { return t.Reason + ": " + t.Err.Error() }
func (t *Terminal) Unwrap() error { return t.Err }

func Fatal(reason string, err error) error { return &Terminal{Reason: reason, Err: err} }

// Declined marks work the system knowingly does not support. It is NOT a
// failure: the document is recorded SKIPPED and the message is acked. Mixing
// "we can't parse this" with "this broke" makes the DLQ unactionable (§2.5).
type Declined struct {
	Reason string
	DocID  string
}

func (d *Declined) Error() string { return "declined: " + d.Reason }

func Decline(docID, reason string) error { return &Declined{Reason: reason, DocID: docID} }

// Handler processes one message. Returning nil acks it.
type Handler func(ctx context.Context, msg jetstream.Msg) error

type Runtime struct {
	Name    string
	Bus     *bus.Bus
	Docs    *postgres.DocumentRepo
	Log     *slog.Logger
	Batch   int
	Subject string
}

// Consume runs the pull loop until ctx is cancelled.
//
// Batch sizing carries policy: CPU-bound work fetches one task at a time so
// idle workers steal from busy ones, while I/O-bound work fetches in batches.
func (r *Runtime) Consume(ctx context.Context, consumer jetstream.Consumer, h Handler) error {
	batch := r.Batch
	if batch < 1 {
		batch = 1
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		msgs, err := consumer.Fetch(batch, jetstream.FetchMaxWait(5*time.Second))
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			r.Log.Warn("fetch failed", "error", err)
			time.Sleep(time.Second)
			continue
		}

		for msg := range msgs.Messages() {
			r.handle(ctx, msg, h)
		}
		if err := msgs.Error(); err != nil && ctx.Err() == nil {
			r.Log.Warn("fetch batch error", "error", err)
		}
	}
}

func (r *Runtime) handle(ctx context.Context, msg jetstream.Msg, h Handler) {
	start := time.Now()
	err := h(ctx, msg)
	telemetry.IngestionDuration.WithLabelValues(r.Name, r.Subject).Observe(time.Since(start).Seconds())

	switch {
	case err == nil:
		telemetry.IngestionTasks.WithLabelValues(r.Name, "completed").Inc()
		_ = msg.Ack()
		return

	case isDeclined(err):
		var d *Declined
		errors.As(err, &d)
		telemetry.IngestionTasks.WithLabelValues(r.Name, "skipped").Inc()
		telemetry.Skipped.WithLabelValues(d.Reason).Inc()
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
		if derr := r.Bus.ToDLQ(ctx, r.Subject, r.Name, reason,
			msg.Headers().Get(bus.HdrTraceparent), deliveries, msg.Data()); derr != nil {
			r.Log.Error("dlq publish failed", "error", derr)
		}
		telemetry.IngestionTasks.WithLabelValues(r.Name, "dlq").Inc()
		telemetry.DLQ.WithLabelValues(r.Name, reason).Inc()

		if id := docIDOf(err); id != "" && r.Docs != nil {
			_ = r.Docs.UpdateStatus(ctx, id, domain.StatusFailed, reason)
		}
		r.Log.Error("terminal failure, routed to dlq",
			"error", err, "deliveries", deliveries, "reason", reason)
		_ = msg.Term()
		return
	}

	telemetry.IngestionTasks.WithLabelValues(r.Name, "retry").Inc()
	r.Log.Warn("transient failure, will redeliver",
		"error", err, "delivery", deliveries, "max", bus.MaxDeliver)
	_ = msg.Nak()
}

func isDeclined(err error) bool {
	var d *Declined
	return errors.As(err, &d)
}

func isTerminal(err error) bool {
	var t *Terminal
	return errors.As(err, &t)
}

func reasonOf(err error) string {
	var t *Terminal
	if errors.As(err, &t) {
		return t.Reason
	}
	return domain.ReasonExtractionFailed
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
