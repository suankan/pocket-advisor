package provision

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/notification"

	"github.com/suankan/pocket-advisor/internal/config"
)

// notifyTargetARN names the target the Tenant declares through its environment
// as _PRIMARY. A bucket rule naming this ARN is what actually causes events to
// publish.
//
// Two things about this string are load-bearing and neither is guessable:
//
//   - The partition is "rustfs", not "minio". RustFS implements MinIO's admin
//     wire protocol almost everywhere, and this is one place the rebrand
//     leaks: it rejects "arn:minio:..." with a message about TargetID format
//     that never mentions the partition.
//   - The target id is lowercase "primary" even though the target is declared
//     as RUSTFS_NOTIFY_NATS_*_PRIMARY. RustFS lowercases the env suffix when
//     it registers the target, and the runtime lookup is case-sensitive.
//
// The second fails in the worst way: SetBucketNotification validates ARN
// *shape* only, so an uppercase id is accepted, stored, and returned by
// GetBucketNotification — and the mismatch surfaces only per-event, in the
// RustFS log, as "Matched notify target is missing from runtime".
const notifyTargetARN = "arn:rustfs:sqs::primary:nats"

// ensureBucketNotification tells this workspace's bucket to publish object
// creations to its NATS target.
//
// The last piece of setup that is not a manifest. The Tenant CRD declares
// buckets, users and policies, but nothing about which bucket publishes where,
// so this remains an S3 call.
//
// Made as the workspace's own identity, not an administrative one — its Tenant
// policy grants s3:* on its own bucket, which covers this. That is what let
// the root credentials leave the binary's configuration entirely: nothing it
// does now requires them.
//
// Scoped to raw/ deliberately: extracted/ children are written by the email
// worker itself, and re-ingesting them would loop.
func ensureBucketNotification(ctx context.Context, cfg *config.Config, id string, log *slog.Logger) error {
	w, err := cfg.Workspace(id)
	if err != nil {
		return err
	}

	// Short per-attempt timeouts. This can run against a tenant that is still
	// starting, and with minio-go's default transport a doomed attempt blocks
	// ~30s on a TCP connect, so a retry loop never gets to retry.
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		ResponseHeaderTimeout: 5 * time.Second,
	}
	c, err := minio.New(w.RustFSEndpoint, &minio.Options{
		Creds:     credentials.NewStaticV4(w.RustFSAccessKey, w.RustFSSecretKey, ""),
		Secure:    cfg.RustFS.UseSSL,
		Transport: transport,
	})
	if err != nil {
		return fmt.Errorf("rustfs client: %w", err)
	}

	arn, err := notification.NewArnFromString(notifyTargetARN)
	if err != nil {
		return fmt.Errorf("notify target arn %q: %w", notifyTargetARN, err)
	}
	queue := notification.NewConfig(arn)
	queue.AddEvents(notification.ObjectCreatedAll)
	queue.AddFilterPrefix("raw/")

	conf := notification.Configuration{}
	conf.AddQueue(queue)

	deadline := time.Now().Add(90 * time.Second)
	for attempt := 1; ; attempt++ {
		err := c.SetBucketNotification(ctx, w.BucketName, conf)
		if err == nil {
			log.Debug("bucket notification set", "workspace_id", id, "bucket", w.BucketName)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("set bucket notification on %s (after %d attempts): %w\n"+
				"  the chart declares one RustFS Tenant per workspace — "+
				"run `make deploy-infra` after adding a workspace to %s",
				w.BucketName, attempt, err, cfg.WorkspacesValuesPath)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}
