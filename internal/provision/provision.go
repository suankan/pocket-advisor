// Package provision prepares the two things a workspace needs that its Helm
// chart cannot declare.
//
// Everything else moved out. The Postgres database and role, the RustFS bucket
// and identity, the NATS account and its JetStream streams are all CRDs now —
// a Cluster, a Tenant and three Streams, reconciled by their operators from
// charts/pocket-advisor-infra. What used to be CREATE ROLE, CREATE DATABASE,
// GRANT, AddCannedPolicy, AddUser, AttachPolicy, EnsureBucket, ConfigMap
// patching and a NATS reload is now a values file (ingestion-design.md
// deviations 20, 22, 24).
//
// Two things could not follow, for the same underlying reason: they are not
// expressible in a manifest.
//
//  1. The schema. Its vector column is halfvec(N), and N comes from probing
//     the embedding endpoint on the operator's own machine — which nothing
//     inside the cluster can reach (§4.4).
//  2. The bucket notification rule. The Tenant CRD declares buckets, users and
//     policies, but has no field for which bucket publishes to which target,
//     so it stays an S3 API call.
//
// Both are idempotent and cheap — a probe, a SELECT and one S3 call — which is
// why --ingest-all simply runs them rather than requiring a separate
// provisioning step. Neither needs administrative credentials: the schema is
// applied as the workspace's own Postgres role, and the notification rule as
// its own RustFS identity, which its Tenant policy already grants.
package provision

import (
	"context"
	"log/slog"

	"github.com/suankan/pocket-advisor/internal/client/embedding"
	"github.com/suankan/pocket-advisor/internal/config"
)

// EnsureWorkspace makes a workspace ready to ingest into.
//
// Safe to call on every run: both steps do nothing when they are already
// satisfied. It assumes the infrastructure exists — if the chart has not been
// deployed for this workspace, the errors say so and name the values file.
// The caller supplies the embedding endpoint's answer rather than this
// package asking for it: every mode that calls EnsureWorkspace already probes
// to verify the index dimension, and probing twice for one startup is work
// nobody asked for.
func EnsureWorkspace(ctx context.Context, cfg *config.Config, id string, info embedding.ModelInfo, log *slog.Logger) error {
	if err := ensureSchema(ctx, cfg, id, info, log); err != nil {
		return err
	}
	return ensureBucketNotification(ctx, cfg, id, log)
}
