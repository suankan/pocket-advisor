// Package provision applies the workspace schema after probing the local
// embedding dimension. The host wrapper provisions databases and NATS streams.
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
