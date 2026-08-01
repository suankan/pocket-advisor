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

// bucketNotification mirrors the payload the deleted pre-4.0.0 webhook
// handler parsed (cmd/discovery/main.go, removed in the monolith refactor).
// RustFS's NATS target publishes the identical Records[].s3.object.* shape,
// including the same form-URL-encoded key — live-verified against beta.12
// with a nested raw/<shard>/<hash> key, not assumed from the old code.
type bucketNotification struct {
	Records []struct {
		S3 struct {
			Object struct {
				Key string `json:"key"`
			} `json:"object"`
		} `json:"s3"`
	} `json:"Records"`
}

func (w *RustFSNotifyWorker) Handle(ctx context.Context, msg jetstream.Msg) error {
	var event bucketNotification
	if err := json.Unmarshal(msg.Data(), &event); err != nil {
		telemetry.DiscoveryFiles.WithLabelValues("notify", "malformed").Inc()
		return Fatal("MALFORMED_NOTIFY_EVENT", err)
	}

	for _, rec := range event.Records {
		key, err := url.QueryUnescape(rec.S3.Object.Key)
		if err != nil {
			telemetry.DiscoveryFiles.WithLabelValues("notify", "malformed").Inc()
			return Fatal("MALFORMED_NOTIFY_EVENT", err)
		}
		if _, err := domain.ParseRawObjectKey(key); err != nil {
			// extracted/ children arrive here too if the bucket rule wasn't
			// scoped to a raw/ prefix. They are owned by the worker that
			// created them and must never mint root documents.
			telemetry.DiscoveryFiles.WithLabelValues("notify", "ignored").Inc()
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
