package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/suankan/pocket-advisor/internal/app"
	"github.com/suankan/pocket-advisor/internal/client/embedding"
	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/telemetry"
	"github.com/suankan/pocket-advisor/internal/uploader"
	"github.com/suankan/pocket-advisor/internal/workspace"
)

// runReset handles --delete-data and --forget.
//
// Both need the uploader identity, the only one permitted to delete, and both
// cascade into Tier 2 — Tier 2 and Tier 3 are derivatives, so purging the
// bucket alone would leave every citation resolving to nothing (§5.1).
func runReset(o *Options, cfg *config.Config, logs *telemetry.Logs) error {
	ctx := context.Background()

	a, err := app.New(ctx, cfg, logs, app.Needs{Uploader: true, Postgres: true}, o.WorkspaceID)
	if err != nil {
		return err
	}
	defer a.Close()

	// Resolve the registry first: a typo in the workspace id should fail before
	// anything is deleted, not after.
	ws, err := workspace.Load(o.WorkspaceConfig, o.WorkspaceID)
	if err != nil {
		return err
	}

	reset := uploader.NewResetter(a.Uploads, a.Docs, a.Logger(telemetry.RoleUploader))

	if o.Forget != "" {
		sha := strings.ToLower(o.Forget)
		if !o.Yes && !confirm(fmt.Sprintf(
			"Remove document %s from workspace %q?\n"+
				"  - deletes its Tier 1 raw and extracted objects\n"+
				"  - deletes its documents row and every chunk derived from it",
			sha, ws.ID)) {
			return errAborted
		}
		if err := reset.Forget(ctx, ws.ID, sha); err != nil {
			return err
		}
		fmt.Printf("forgotten: workspace=%s sha256=%s\n", ws.ID, sha)
		return nil
	}

	counts, err := describe(ctx, a.Docs, ws.ID)
	if err != nil {
		// A summary is a courtesy; failing to produce one is not a reason to
		// refuse the operation, but the prompt must not imply it counted.
		counts = "unable to count existing rows"
	}

	if !o.Yes && !confirm(fmt.Sprintf(
		"DELETE ALL DATA for workspace %q (%d collections)?\n"+
			"  - %s\n"+
			"  - deletes every object in this workspace's bucket\n"+
			"  - deletes every documents row and every chunk for the workspace\n"+
			"This cannot be undone. Tier 1 is the source of truth; re-ingesting\n"+
			"requires the original files still being on disk.",
		ws.ID, len(ws.Collections), counts)) {
		return errAborted
	}

	if err := reset.Wipe(ctx, ws.ID); err != nil {
		return err
	}
	fmt.Printf("deleted: workspace=%s\n", ws.ID)
	return nil
}

func describe(ctx context.Context, docs *postgres.DocumentRepo, workspaceID string) (string, error) {
	known, err := docs.KnownRawURIs(ctx, workspaceID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d document(s) currently indexed", len(known)), nil
}

// runBootstrap resolves the vector dimension from the embedding endpoint and
// (re-)applies the DDL to one workspace's own database.
//
// halfvec(N) is a typed SQL column, so N must be known before the first CREATE
// TABLE — but the authority on N is the model, not a design document. Pinning a
// literal in checked-in DDL is how an index silently ends up the wrong shape
// when the endpoint changes (§4.4). --create-workspace already runs this once
// as part of provisioning (workspace-isolation.md §6) — this mode exists for
// re-probing an existing workspace after a model change.
func runBootstrap(o *Options, cfg *config.Config, logs *telemetry.Logs) error {
	ctx := context.Background()

	a, err := app.New(ctx, cfg, logs, app.Needs{Postgres: true}, o.WorkspaceID)
	if err != nil {
		return err
	}
	defer a.Close()

	if err := cfg.RequireEmbedding(); err != nil {
		return err
	}
	client := embedding.New(cfg.Embedding)

	info, err := client.Probe(ctx)
	if err != nil {
		return fmt.Errorf("probe %s: %w", cfg.Embedding.Endpoint, err)
	}
	model := cfg.Embedding.Model
	if info.Model != "" {
		model = info.Model
	}
	a.Log.Info("probed embedding endpoint", "model", model, "dimension", info.Dimension)

	if err := a.DB.ApplySchema(ctx, postgres.SchemaMetadata{
		EmbedModel: model,
		EmbedDim:   info.Dimension,
	}); err != nil {
		return err
	}

	fmt.Printf("schema ready: model=%s dimension=%d\n", model, info.Dimension)
	return nil
}
