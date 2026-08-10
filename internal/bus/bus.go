// Package bus wraps NATS JetStream: stream provisioning, typed publishing,
// and the DLQ protocol that JetStream does not provide natively
// (ingestion-design.md §2.5).
package bus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

// Subjects. Every subject a producer can emit to must have a consumer, or
// work accumulates silently.
const (
	SubjectEmails = "ingest.emails.raw"
	SubjectPDFs   = "ingest.pdfs.raw"
	SubjectDocx   = "ingest.docx.raw"
	SubjectImages = "ingest.images.raw"
	SubjectEmbed  = "ingest.text.embed"
	SubjectDLQ    = "ingest.dlq"

	// SubjectRustFSEvents carries RustFS's own S3-shaped ObjectCreated/
	// ObjectUpdated event JSON, published directly by RustFS's native NATS
	// notify target — not a protobuf command, so it lives on its own stream
	// rather than StreamName (ingestion-design.md §5.2).
	SubjectRustFSEvents = "rustfs.events.raw"

	StreamName = "INGESTION"
	StreamDLQ  = "INGESTION_DLQ"
	// StreamRustFSEvents holds RustFS's raw notify events. Separate from
	// StreamName because it needs a much wider duplicate window: RustFS
	// refuses to publish into a stream whose dedup window doesn't cover its
	// own retry lifetime (~274s observed live), which StreamName's default
	// does not need and should not carry.
	StreamRustFSEvents = "RUSTFS_EVENTS"
	// RustFSEventsDedupWindow must exceed RustFS's retry lifetime — live-
	// verified against beta.12, which refused to publish into a stream with
	// the 2-minute default. 10m leaves headroom.
	RustFSEventsDedupWindow = 10 * time.Minute

	// MaxDeliver bounds redelivery; the third attempt routes to the DLQ.
	MaxDeliver = 3
	// AckWait is how long the broker waits before concluding a worker died.
	//
	// It no longer has to cover the slowest document as well. It used to, and
	// could not: a 208-page scanned PDF needs ~15 minutes of OCR, so a 5-minute
	// window redelivered it mid-flight and MaxDeliver eventually dead-lettered a
	// document nothing was wrong with. Runtime.heartbeat now calls InProgress
	// while a handler runs, which holds the deadline open for exactly as long as
	// real work is happening.
	//
	// So this is sized for crash detection alone. It was briefly 10 minutes,
	// as margin in case the heartbeat missed a tick — but that margin was
	// really covering a heartbeat that gave up permanently on its first failed
	// InProgress. Fixing the heartbeat to keep trying removed the reason, and
	// the shorter window is the better one: it halves how long a genuinely dead
	// worker's message sits before someone else can have it.
	AckWait = 5 * time.Minute
)

// DLQ headers, so a message in the dead letter queue is diagnosable without
// re-running the pipeline.
const (
	HdrFailureReason = "X-Failure-Reason"
	HdrFailureWorker = "X-Failure-Worker"
	HdrDeliveryCount = "X-Delivery-Count"
	HdrTraceparent   = "X-Traceparent"
	HdrOrigSubject   = "X-Original-Subject"
)

type Bus struct {
	nc          *nats.Conn
	js          jetstream.JetStream
	workspaceID string
}

// Connect opens an anonymous NATS connection scoped to one workspace.
// There is no NATS account, user, or password any more — isolation moves to
// subject and stream naming instead, namespaced by workspaceID and applied
// internally by every method below, so no other package needs to know the
// namespacing scheme exists (workspace-isolation.md §2.3).
func Connect(ctx context.Context, url, workspaceID string) (*Bus, error) {
	nc, err := nats.Connect(url,
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("nats connect %s: %w", url, err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	return &Bus{nc: nc, js: js, workspaceID: workspaceID}, nil
}

// subject namespaces a bare subject constant to this connection's workspace.
// Dots are NATS's own subject hierarchy separator, and no workspace id
// contains one, so a plain prefix is unambiguous.
func (b *Bus) subject(bare string) string {
	return b.workspaceID + "." + bare
}

// stream namespaces a bare stream-name constant to this connection's
// workspace. Stream names, unlike subjects, disallow dots — this reuses the
// same hyphen-to-underscore, uppercased suffix transform
// charts/pocket-advisor-infra/templates/rustfs.yaml already applies to
// workspace ids for its per-workspace notify env var names, so the two stay
// visually consistent even though nothing enforces they must.
func (b *Bus) stream(bare string) string {
	suffix := strings.ToUpper(strings.ReplaceAll(b.workspaceID, "-", "_"))
	return bare + "_" + suffix
}

func (b *Bus) Close() { b.nc.Close() }

// PurgeQueues empties this workspace's streams.
//
// Queues are the one tier that is purely derived: a command names a Tier 1
// object and a document row, and once both are gone the command describes work
// that cannot be done. Left behind after a wipe they are worse than useless —
// every one of them fails on a missing object and manufactures a fresh dead
// letter about a document nobody has any more.
//
// The DLQ goes too, and that is the point of purging at all rather than letting
// maxAge expire it: its 132 entries after one wipe describe a corpus that no
// longer exists, so anything reading it to decide what needs attention is
// reading history as if it were a work list.
//
// Non-fatal by design. Every stream is attempted even if an earlier one fails,
// and the errors are returned together, because a stale queue is untidy while a
// half-purged one is no worse than the state this started from.
func (b *Bus) PurgeQueues(ctx context.Context) error {
	var failed []string
	for _, name := range []string{b.stream(StreamName), b.stream(StreamDLQ), b.stream(StreamRustFSEvents)} {
		s, err := b.js.Stream(ctx, name)
		if err != nil {
			// A stream that does not exist is already as empty as it can be.
			if errors.Is(err, jetstream.ErrStreamNotFound) {
				continue
			}
			failed = append(failed, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if err := s.Purge(ctx); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", name, err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("purge queues: %s", strings.Join(failed, "; "))
	}
	return nil
}

// EnsureStreams provisions the work queue and the DLQ. Idempotent, so every
// binary can call it at startup.
func (b *Bus) EnsureStreams(ctx context.Context) error {
	_, err := b.js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     b.stream(StreamName),
		Subjects: []string{b.subject(SubjectEmails), b.subject(SubjectPDFs), b.subject(SubjectDocx), b.subject(SubjectImages), b.subject(SubjectEmbed)},
		// WorkQueue: a message is deleted once acked, so the stream holds
		// backlog rather than history. Sizing follows from that (§6.3).
		Retention: jetstream.WorkQueuePolicy,
		Storage:   jetstream.FileStorage,
		Discard:   jetstream.DiscardNew, // reject new rather than drop pending
		MaxMsgs:   1_000_000,
	})
	if err != nil {
		return fmt.Errorf("create stream %s: %w", b.stream(StreamName), err)
	}

	// The DLQ is a separate stream with limits retention: its whole purpose is
	// to retain messages for inspection after they stop being work.
	_, err = b.js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      b.stream(StreamDLQ),
		Subjects:  []string{b.subject(SubjectDLQ)},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.FileStorage,
		MaxAge:    30 * 24 * time.Hour,
	})
	if err != nil {
		return fmt.Errorf("create stream %s: %w", b.stream(StreamDLQ), err)
	}

	// RustFS's live event feed (§5.2) — WorkQueue like StreamName, for the
	// same reason (a durable pull consumer drains it), but its own stream so
	// its wider dedup window doesn't apply to the typed-command traffic.
	_, err = b.js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:       b.stream(StreamRustFSEvents),
		Subjects:   []string{b.subject(SubjectRustFSEvents)},
		Retention:  jetstream.WorkQueuePolicy,
		Storage:    jetstream.FileStorage,
		Discard:    jetstream.DiscardNew,
		MaxMsgs:    1_000_000,
		Duplicates: RustFSEventsDedupWindow,
	})
	if err != nil {
		return fmt.Errorf("create stream %s: %w", b.stream(StreamRustFSEvents), err)
	}
	return nil
}

// Publish sends a protobuf command and waits for the PubAck.
//
// subject is the bare package constant (e.g. SubjectEmails) — Publish
// namespaces it to this connection's workspace internally, so callers never
// handle the namespaced form themselves.
//
// Never fire-and-forget: the write-then-publish gap (§2.2) is only survivable
// if a failed publish is actually observed.
func (b *Bus) Publish(ctx context.Context, subject string, msg proto.Message, traceparent string) error {
	subject = b.subject(subject)
	payload, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", subject, err)
	}
	m := &nats.Msg{
		Subject: subject,
		Data:    payload,
		Header:  nats.Header{},
	}
	if traceparent != "" {
		m.Header.Set(HdrTraceparent, traceparent)
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := b.js.PublishMsg(ctx, m); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 200 * time.Millisecond):
		}
	}
	return fmt.Errorf("publish %s after retries: %w", subject, lastErr)
}

// ToDLQ republishes a failed message with diagnostic headers.
//
// JetStream has no native dead letter queue — it offers MaxDeliver plus an
// advisory. This is that missing half, and it is application code by
// necessity (§2.5).
func (b *Bus) ToDLQ(ctx context.Context, origSubject, worker, reason, traceparent string, deliveries uint64, payload []byte) error {
	m := &nats.Msg{
		Subject: b.subject(SubjectDLQ),
		Data:    payload,
		Header:  nats.Header{},
	}
	m.Header.Set(HdrFailureReason, reason)
	m.Header.Set(HdrFailureWorker, worker)
	m.Header.Set(HdrDeliveryCount, fmt.Sprintf("%d", deliveries))
	m.Header.Set(HdrOrigSubject, b.subject(origSubject))
	if traceparent != "" {
		m.Header.Set(HdrTraceparent, traceparent)
	}
	if _, err := b.js.PublishMsg(ctx, m); err != nil {
		return fmt.Errorf("publish to dlq: %w", err)
	}
	return nil
}

// PullConsumer creates a durable pull consumer for one subject on stream.
// stream and subject are the bare package constants — namespaced internally,
// same as Publish.
func (b *Bus) PullConsumer(ctx context.Context, stream, durable, subject string) (jetstream.Consumer, error) {
	stream, subject = b.stream(stream), b.subject(subject)
	c, err := b.js.CreateOrUpdateConsumer(ctx, stream, jetstream.ConsumerConfig{
		Durable:       durable,
		FilterSubject: subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
		MaxDeliver:    MaxDeliver,
		AckWait:       AckWait,
	})
	if err != nil {
		return nil, fmt.Errorf("consumer %s on %s: %w", durable, subject, err)
	}
	return c, nil
}

// Pending reports queued messages for a subject. The scan uses this to avoid
// outrunning the pipeline (§5.2).
func (b *Bus) Pending(ctx context.Context) (uint64, error) {
	s, err := b.js.Stream(ctx, b.stream(StreamName))
	if err != nil {
		return 0, err
	}
	info, err := s.Info(ctx)
	if err != nil {
		return 0, err
	}
	return info.State.Msgs, nil
}

// StreamInfo reports info for a bare stream-name constant (e.g. StreamName,
// StreamDLQ), namespaced to this connection's workspace internally — same
// as Publish, callers never handle the namespaced form themselves. Doctor
// checks are the reason this is exported: they inspect more than one
// stream's message counts directly, unlike the pipeline which only ever
// needs PullConsumer/Publish.
func (b *Bus) StreamInfo(ctx context.Context, bareName string) (*jetstream.StreamInfo, error) {
	s, err := b.js.Stream(ctx, b.stream(bareName))
	if err != nil {
		return nil, err
	}
	return s.Info(ctx)
}
