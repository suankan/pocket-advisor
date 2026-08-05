package worker

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/telemetry"
)

// Ingester is the one discovery.Service method this worker needs — narrowed
// to an interface so it's testable without a real Postgres/RustFS/NATS-backed
// Service, the same reason the pre-4.0.0 webhook handler had its own
// notifyIngester interface.
type Ingester interface {
	Ingest(ctx context.Context, workspaceID, key, mode string) error
}

// RustFSNotifyWorker translates RustFS's native S3-shaped notify events
// (ingestion-design.md §5.2) into calls to discovery.Service.Ingest — the
// same function Scan and Reconcile already call, so a live event and a
// rediscovered gap are handled identically. It lives here rather than in
// internal/discovery because internal/worker already imports discovery
// (EmailWorker uses discovery.Classify); the reverse import would cycle.
//
// WorkspaceID is set once at construction, not parsed per-event: object
// keys carry no workspace segment (each workspace has its own bucket, which
// already provides that scoping), and this worker is itself always built
// scoped to one workspace's Vault/Docs/Bus (§5.2, pipeline.New's
// opts.RustFSEvents). Workspace identity comes from which bucket you're
// connected to, not a string embedded in a key.
type RustFSNotifyWorker struct {
	Discovery   Ingester
	WorkspaceID string
	Log         *slog.Logger
}

// bucketNotification mirrors what RustFS's NATS target actually publishes.
//
// It is NOT the plain S3 shape the pre-4.0.0 webhook handler parsed. RustFS
// wraps each record, putting the S3 event one level down under "data":
//
//	{"Records":[{"object_name":"raw%2F..","data":{"s3":{"object":{"key":..}}}}]}
//
// An earlier revision of this struct read Records[].s3.object.key, matching
// the webhook payload. Against this one that path simply does not exist, so
// encoding/json left Key empty — it ignores unknown fields and reports no
// error — every key failed ParseRawObjectKey, and each message was counted
// ignored and acked. The observable result was a queue draining at full speed
// while creating nothing: 79 events "done", zero documents, zero errors, zero
// DLQ. Taken from a live beta.12 message, not from the webhook code.
//
// ObjectName is the fallback because it carries the same key at the top of the
// record; between the two, a future payload change has to break both to go
// unnoticed again.
type bucketNotification struct {
	Records []struct {
		ObjectName string `json:"object_name"`
		Data       struct {
			S3 struct {
				Object struct {
					Key string `json:"key"`
				} `json:"object"`
			} `json:"s3"`
		} `json:"data"`
	} `json:"Records"`
}

// key returns the object key a record refers to, preferring the nested S3
// shape and falling back to the record's own object_name.
func (r *bucketNotification) key(i int) string {
	if k := r.Records[i].Data.S3.Object.Key; k != "" {
		return k
	}
	return r.Records[i].ObjectName
}

func (w *RustFSNotifyWorker) Handle(ctx context.Context, msg jetstream.Msg) error {
	var event bucketNotification
	if err := json.Unmarshal(msg.Data(), &event); err != nil {
		telemetry.DiscoveryFiles.WithLabelValues("notify", "malformed").Inc()
		return Fatal(domain.ReasonMalformedNotify, err)
	}

	if len(event.Records) == 0 {
		// Never expected: the target publishes one record per object event.
		// Logged rather than ignored because an empty Records is exactly what
		// a payload-shape change looks like from in here, and the previous
		// shape mismatch stayed invisible for want of this line.
		telemetry.DiscoveryFiles.WithLabelValues("notify", "malformed").Inc()
		w.Log.Warn("notify event carried no records", "payload", string(msg.Data()))
		return nil
	}

	for i := range event.Records {
		key, err := url.QueryUnescape(event.key(i))
		if err != nil {
			telemetry.DiscoveryFiles.WithLabelValues("notify", "malformed").Inc()
			return Fatal(domain.ReasonMalformedNotify, err)
		}
		if _, err := domain.ParseRawObjectKey(key); err != nil {
			// extracted/ children arrive here too if the bucket rule wasn't
			// scoped to a raw/ prefix. They are owned by the worker that
			// created them and must never mint root documents.
			telemetry.DiscoveryFiles.WithLabelValues("notify", "ignored").Inc()
			w.Log.Debug("ignoring notify event for a non-raw key", "key", key)
			continue
		}
		if err := w.Discovery.Ingest(ctx, w.WorkspaceID, key, "notify"); err != nil {
			// Not wrapped with WithDoc: Ingest can fail before a doc_id exists
			// (e.g. Vault.Get), and one message may carry several Records, so
			// there is no single document this failure belongs to. Retryable —
			// the object store or database may recover.
			return err
		}
	}
	return nil
}
