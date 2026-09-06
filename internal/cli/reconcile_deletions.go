package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/suankan/pocket-advisor/internal/app"
	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/telemetry"
	"github.com/suankan/pocket-advisor/internal/uploader"
	"github.com/suankan/pocket-advisor/internal/workspace"
)

// runReconcileDeletions removes documents nothing stages any more.
//
// It is the same sweep --ingest-all runs before its pools start, reached
// explicitly and without ingesting anything: the way to look at a plan, and
// the way to apply one that --ingest-all's blast-radius guard refused. The
// guard is off here because the operator typed the command and is shown the
// plan before being asked — that is a better check than a ratio.
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

	// RustFS as well as Uploader: the containment backfill reads every
	// container's Tier 1 bytes back, and the read-only worker role is the right
	// identity for a pass that only reads.
	a, err := app.New(ctx, cfg, logs,
		app.Needs{RustFS: true, Uploader: true, Postgres: true, NATS: true}, o.WorkspaceID)
	if err != nil {
		return err
	}
	defer a.Close()

	log := a.Logger(telemetry.RoleUploader)
	if err := ensureContainment(ctx, a, log); err != nil {
		return err
	}

	reset := uploader.NewResetter(a.Uploads, a.Docs, a.Bus, log)
	pruner := uploader.NewPruner(a.Docs, reset, log)

	report, err := pruner.Plan(ctx, uploader.PruneOptions{
		AbsPaths: ws.AbsPaths,
		Ignore:   ws.Ignore.Match,
		MaxShare: 1,
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
		if o.Yes && !o.DryRun {
			if err := reset.DrainPending(ctx); err != nil {
				return err
			}
		}
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
			"  - deletes their Tier 1 objects, their rows and every chunk derived from them\n"+
			"Re-ingesting requires source files included by the workspace configuration.",
		len(report.Candidates), ws.ID, report.Roots, report.Contained)) {
		return errAborted
	}
	if err := reset.DrainPending(ctx); err != nil {
		return err
	}

	if err := pruner.Apply(ctx, ws.ID, report); err != nil {
		return err
	}
	fmt.Printf("deleted: workspace=%s documents=%d\n", ws.ID, report.Deleted)
	return nil
}
