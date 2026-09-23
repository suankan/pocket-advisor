package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/suankan/pocket-advisor/internal/app"
	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/retention"
	"github.com/suankan/pocket-advisor/internal/telemetry"
	"github.com/suankan/pocket-advisor/internal/workspace"
)

// runReconcileDeletions removes documents nothing stages any more.
//
// It is the only sweep. Ingestion is additive, so deletion happens here, in
// its own process under the maintenance lock, where no worker can add a
// document or an edge while the plan is judged. It takes its own source
// inventory and admits nothing. The operator is shown the plan and must
// confirm it; that review is the check against a directory that failed to
// mount, beyond the refusal of an empty one.
func runReconcileDeletions(o *Options, cfg *config.Config, logs *telemetry.Logs) error {
	ctx := context.Background()

	// Resolve the registry before opening a store: a mistyped workspace should
	// fail before anything is read, let alone deleted. Load also proves the
	// directories exist, so a missing mount is an error here rather than an
	// empty walk that looks like mass deletion.
	ws, err := workspace.Load(o.WorkspaceConfig, o.WorkspaceID)
	if err != nil {
		return err
	}

	// Sources as well as Postgres: the containment backfill reads every
	// container's Tier 1 bytes back through the verified resolver, which only
	// ever opens configured originals read-only.
	a, err := app.New(ctx, cfg, logs,
		app.Needs{Sources: true, Postgres: true, NATS: true}, o.WorkspaceID)
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
	if err := discoveryService(a).BindExisting(ctx); err != nil {
		return err
	}

	log := a.Logger(telemetry.RoleRetention)
	if err := ensureContainment(ctx, a, log); err != nil {
		return err
	}

	pruner := retention.NewPruner(a.Docs, log)

	report, err := pruner.Plan(ctx, retention.PruneOptions{
		AbsPaths: ws.AbsPaths,
		Ignore:   ws.Ignore.Match,
	})
	if err != nil {
		return err
	}

	if o.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
	} else {
		renderPrunePlan(report, ws.ID)
	}

	if len(report.Candidates) == 0 {
		if !o.JSON {
			fmt.Printf("\nEverything indexed is still staged. Nothing to reconcile.\n")
		}
		return nil
	}
	if !o.Yes || o.DryRun {
		fmt.Printf("\nNo documents were deleted. Re-run with --yes to apply.\n")
		return nil
	}
	if !confirm(fmt.Sprintf(
		"Delete %d document(s) from workspace %q?\n"+
			"  - %d were staged in their own right; %d are held by nothing else\n"+
			"  - deletes derived rows and unreferenced passages; preserves original files\n"+
			"Re-ingesting requires source files included by the workspace configuration.",
		len(report.Candidates), ws.ID, report.Roots, report.Contained)) {
		return errAborted
	}

	if err := pruner.Apply(ctx, ws.ID, report); err != nil {
		return err
	}
	fmt.Printf("deleted: workspace=%s documents=%d\n", ws.ID, report.Deleted)
	return nil
}
