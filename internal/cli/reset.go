package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/suankan/pocket-advisor/internal/app"
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

// --bootstrap-schema lived here and was removed. It duplicated
// provision.applyWorkspaceSchema step for step — workspace DSN, RequireEmbedding,
// Probe, resolve model, ApplySchema — without calling it, and every use it
// covered is covered elsewhere: --create-workspace applies the schema on every
// run, idempotently, and VerifyDimension catches endpoint drift at the start of
// --ingest-all and --listen rather than when someone remembers to check.
