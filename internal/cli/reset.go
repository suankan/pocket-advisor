package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/suankan/pocket-advisor/internal/app"
	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/retention"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/telemetry"
	"github.com/suankan/pocket-advisor/internal/workspace"
)

// runReset removes derived state while leaving all original files untouched.
func runReset(o *Options, cfg *config.Config, logs *telemetry.Logs) error {
	ctx := context.Background()

	a, err := app.New(ctx, cfg, logs, app.Needs{Sources: true, Postgres: true}, o.WorkspaceID)
	if err != nil {
		return err
	}
	defer a.Close()
	unlock, err := a.DB.LockMaintenance(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err := a.DB.ApplySourceSchema(ctx); err != nil {
		return err
	}

	// Resolve the registry first: a typo in the workspace id should fail before
	// anything is deleted, not after.
	ws, err := workspace.Load(o.WorkspaceConfig, o.WorkspaceID)
	if err != nil {
		return err
	}

	reset := retention.NewResetter(a.Docs, a.Logger(telemetry.RoleRetention))

	if o.Forget != "" {
		if err := discoveryService(a).BindExisting(ctx); err != nil {
			return err
		}
		sha := strings.ToLower(o.Forget)
		if !o.Yes && !confirm(fmt.Sprintf(
			"Remove document %s from workspace %q?\n"+
				"  - preserves every original file\n"+
				"  - deletes its row, exclusively held descendants, and their chunks\n"+
				"  - retains descendants still staged independently or held by another container",
			sha, ws.ID)) {
			return errAborted
		}
		if err := ensureContainment(ctx, a, a.Logger(telemetry.RoleRetention)); err != nil {
			return err
		}
		if err := reset.Forget(ctx, ws.ID, sha); err != nil {
			return err
		}
		fmt.Printf("forgotten: workspace=%s sha256=%s\n", ws.ID, sha)
		return nil
	}

	counts, err := describe(ctx, a.Docs)
	if err != nil {
		// A summary is a courtesy; failing to produce one is not a reason to
		// refuse the operation, but the prompt must not imply it counted.
		counts = "unable to count existing rows"
	}

	if !o.Yes && !confirm(fmt.Sprintf(
		"DELETE ALL DATA for workspace %q?\n"+
			"  - %s\n"+
			"  - preserves every original file\n"+
			"  - deletes every documents row and every chunk for the workspace\n"+
			"  - deletes its ingestion job queue: every job names a row that will\n"+
			"    no longer exist\n"+
			"This cannot be undone. Rebuilding derived state\n"+
			"requires the original files still being on disk.",
		ws.ID, counts)) {
		return errAborted
	}

	if err := reset.Wipe(ctx, ws.ID); err != nil {
		return err
	}
	fmt.Printf("deleted: workspace=%s\n", ws.ID)
	return nil
}

func describe(ctx context.Context, docs *postgres.DocumentRepo) (string, error) {
	n, err := docs.CountDocuments(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d document(s) currently indexed", n), nil
}

// --bootstrap-schema lived here and was removed. It duplicated the schema step
// in internal/provision step for step — workspace DSN, RequireEmbedding, Probe,
// resolve model, ApplySchema — without calling it, and every use it covered is
// covered elsewhere: provision.EnsureWorkspace applies the schema idempotently
// at the start of --ingest-all and --listen, and VerifyDimension catches
// endpoint drift there too, rather than when someone remembers to check.
