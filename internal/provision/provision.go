// Package provision prepares the one thing a workspace needs that no
// pocket-advisor.sh command already covers.
//
// Everything else lives in `./pocket-advisor.sh deploy-workspaces` now, not
// here and not in a CRD an operator reconciles — deviation 39 removed
// CloudNativePG, the RustFS operator and NACK entirely, on the view that
// reconciling drift against a desired state, continuously, unattended,
// across many clusters is machinery this single-tenant local cluster has no
// use for. What that command does instead — CREATE ROLE, CREATE DATABASE,
// CREATE EXTENSION over psql; a bucket, an identity and a policy over
// rc/aws-cli; three JetStream streams over natscli — is the same operation
// an operator would have made, run once by a human instead of continuously
// by a controller.
//
// One thing could not follow, and for a real reason rather than because no
// tool existed yet: the schema below. Its vector column is halfvec(N), and N
// comes from probing the embedding endpoint on the operator's own machine —
// which nothing inside the cluster, and no infra tooling running elsewhere,
// can reach (§4.4).
package provision

import (
	"context"
	"log/slog"

	"github.com/suankan/pocket-advisor/internal/client/embedding"
	"github.com/suankan/pocket-advisor/internal/config"
)

// EnsureWorkspace makes a workspace ready to ingest into.
//
// Safe to call on every run: it does nothing when already satisfied. It
// assumes the infrastructure exists — if the chart has not been deployed for
// this workspace, the error says so and names the values file. The caller
// supplies the embedding endpoint's answer rather than this package asking
// for it: every mode that calls EnsureWorkspace already probes to verify the
// index dimension, and probing twice for one startup is work nobody asked
// for.
func EnsureWorkspace(ctx context.Context, cfg *config.Config, id string, info embedding.ModelInfo, log *slog.Logger) error {
	return ensureSchema(ctx, cfg, id, info, log)
}
