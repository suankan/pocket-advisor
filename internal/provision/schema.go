package provision

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/suankan/pocket-advisor/internal/client/embedding"
	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
)

// ensureSchema applies the Tier 2/3 DDL to the workspace's own database, as
// the workspace's own role.
//
// The one piece of workspace setup that cannot be a manifest: the vector
// column is halfvec(N), and N is resolved by asking the embedding endpoint on
// localhost how wide its vectors are. Nothing running in the cluster can reach
// it, so this stays on the host (§4.4).
//
// Idempotent — ApplySchema returns early when the recorded dimension already
// matches, and refuses outright when it does not, since a changed dimension is
// a re-embed rather than a migration.
func ensureSchema(ctx context.Context, cfg *config.Config, id string, info embedding.ModelInfo, log *slog.Logger) error {
	dsn, err := cfg.WorkspacePostgresDSN(id)
	if err != nil {
		return err
	}
	db, err := postgres.Connect(ctx, dsn, 2)
	if err != nil {
		return fmt.Errorf("connect workspace database: %w\n"+
			"  `./pocket-advisor.sh deploy-workspaces` creates every registered "+
			"workspace's database and role — is %q in workspaces/workspaces.yaml, "+
			"and has deploy-workspaces (or deploy-infra) been run since it was added?",
			err, id)
	}
	defer db.Close()

	model := cfg.Embedding.Model
	if info.Model != "" {
		model = info.Model
	}

	if err := db.ApplySchema(ctx, postgres.SchemaMetadata{
		EmbedModel: model,
		EmbedDim:   info.Dimension,
	}); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	log.Debug("workspace schema present", "workspace_id", id, "model", model, "dimension", info.Dimension)
	return nil
}
