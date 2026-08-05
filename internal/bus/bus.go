// Package bus wraps NATS JetStream: stream provisioning, typed publishing,
// and the DLQ protocol that JetStream does not provide natively
// (ingestion-design.md §2.5).
package bus

import (
	"context"
	"fmt"
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
	nc *nats.Conn
	js jetstream.JetStream
}

// Connect authenticates as one workspace's NATS user (workspace-isolation.md
// §2.3) — every account is a fully separate subject space, so this is what
// scopes a connection to its own workspace's streams and nothing else's.
func Connect(ctx context.Context, url, natsUser, natsPassword string) (*Bus, error) {
	nc, err := nats.Connect(url,
		nats.UserInfo(natsUser, natsPassword),
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
	return &Bus{nc: nc, js: js}, nil
}

func (b *Bus) Close() { b.nc.Close() }

func (b *Bus) JS() jetstream.JetStream { return b.js }

// EnsureStreams provisions the work queue and the DLQ. Idempotent, so every
// binary can call it at startup.
func (b *Bus) EnsureStreams(ctx context.Context) error {
	_, err := b.js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     StreamName,
		Subjects: []string{SubjectEmails, SubjectPDFs, SubjectDocx, SubjectImages, SubjectEmbed},
		// WorkQueue: a message is deleted once acked, so the stream holds
		// backlog rather than history. Sizing follows from that (§6.3).
		Retention: jetstream.WorkQueuePolicy,
		Storage:   jetstream.FileStorage,
		Discard:   jetstream.DiscardNew, // reject new rather than drop pending
		MaxMsgs:   1_000_000,
	})
	if err != nil {
		return fmt.Errorf("create stream %s: %w", StreamName, err)
	}

	// The DLQ is a separate stream with limits retention: its whole purpose is
	// to retain messages for inspection after they stop being work.
	_, err = b.js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      StreamDLQ,
		Subjects:  []string{SubjectDLQ},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.FileStorage,
		MaxAge:    30 * 24 * time.Hour,
	})
	if err != nil {
		return fmt.Errorf("create stream %s: %w", StreamDLQ, err)
	}

	// RustFS's live event feed (§5.2) — WorkQueue like StreamName, for the
	// same reason (a durable pull consumer drains it), but its own stream so
	// its wider dedup window doesn't apply to the typed-command traffic.
	_, err = b.js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:       StreamRustFSEvents,
		Subjects:   []string{SubjectRustFSEvents},
		Retention:  jetstream.WorkQueuePolicy,
		Storage:    jetstream.FileStorage,
		Discard:    jetstream.DiscardNew,
		MaxMsgs:    1_000_000,
		Duplicates: RustFSEventsDedupWindow,
	})
	if err != nil {
		return fmt.Errorf("create stream %s: %w", StreamRustFSEvents, err)
	}
	return nil
}

// Publish sends a protobuf command and waits for the PubAck.
//
// Never fire-and-forget: the write-then-publish gap (§2.2) is only survivable
// if a failed publish is actually observed.
func (b *Bus) Publish(ctx context.Context, subject string, msg proto.Message, traceparent string) error {
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
		Subject: SubjectDLQ,
		Data:    payload,
		Header:  nats.Header{},
	}
	m.Header.Set(HdrFailureReason, reason)
	m.Header.Set(HdrFailureWorker, worker)
	m.Header.Set(HdrDeliveryCount, fmt.Sprintf("%d", deliveries))
	m.Header.Set(HdrOrigSubject, origSubject)
	if traceparent != "" {
		m.Header.Set(HdrTraceparent, traceparent)
	}
	if _, err := b.js.PublishMsg(ctx, m); err != nil {
		return fmt.Errorf("publish to dlq: %w", err)
	}
	return nil
}

// PullConsumer creates a durable pull consumer for one subject on stream.
func (b *Bus) PullConsumer(ctx context.Context, stream, durable, subject string) (jetstream.Consumer, error) {
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
	s, err := b.js.Stream(ctx, StreamName)
	if err != nil {
		return 0, err
	}
	info, err := s.Info(ctx)
	if err != nil {
		return 0, err
	}
	return info.State.Msgs, nil
}
