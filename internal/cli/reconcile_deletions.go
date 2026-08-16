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

// runReconcileDeletions removes documents whose staged file is gone.
//
// The staging directory is normally a feed, not an authority: Tier 1 owns the
// bytes, and a workspace directory can be archived or moved without the corpus
// meaning anything different. This mode is the deliberate exception, so it is
// shaped like the other destructive commands rather than like a sync — it
// plans, prints, and only then asks.
func runReconcileDeletions(o *Options, cfg *config.Config, logs *telemetry.Logs) error {
	ctx := context.Background()

	// Resolve the registry before opening a store: a mistyped workspace should
	// fail before anything is read, let alone deleted. Load also proves the
	// directory exists and is a directory, so a missing mount is an error here
	// rather than an empty walk that looks like mass deletion.
	ws, err := workspace.Load(o.WorkspaceConfig, o.WorkspaceID)
	if err != nil {
		return err
	}

	a, err := app.New(ctx, cfg, logs, app.Needs{Uploader: true, Postgres: true, NATS: true}, o.WorkspaceID)
	if err != nil {
		return err
	}
	defer a.Close()

	log := a.Logger(telemetry.RoleUploader)
	reset := uploader.NewResetter(a.Uploads, a.Docs, a.Bus, log)
	reconciler := uploader.NewReconciler(a.Docs, reset, log)

	report, err := reconciler.Plan(ctx, ws.AbsPath)
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
		renderDeletionPlan(report, ws.ID)
	}

	if len(report.Candidates) == 0 {
		return nil
	}
	if !o.Yes {
		fmt.Printf("\nNothing was changed. Re-run with --yes to apply.\n")
		return nil
	}
	if !confirm(fmt.Sprintf(
		"Delete %d document(s) and %d extracted descendant(s) from workspace %q?\n"+
			"  - deletes their Tier 1 raw and extracted objects\n"+
			"  - deletes their documents rows and every chunk derived from them\n"+
			"Re-ingesting requires the original files; the staged copies are gone.",
		len(report.Candidates), report.DescendantsTotal, ws.ID)) {
		return errAborted
	}

	if err := reconciler.Apply(ctx, ws.ID, report); err != nil {
		return err
	}
	fmt.Printf("deleted: workspace=%s documents=%d descendants=%d\n",
		ws.ID, report.Deleted, report.DescendantsTotal)
	return nil
}

// renderDeletionPlan prints what the operator is being asked to approve. The
// staged path is shown because it is the only thing that makes a candidate
// recognisable; it is the operator's own directory, printed on their own
// terminal, and never leaves the process.
func renderDeletionPlan(report *uploader.DeletionReport, workspaceID string) {
	fmt.Printf("workspace:    %s\n", workspaceID)
	fmt.Printf("staged:       %d files, %d distinct contents\n",
		report.StagedFiles, report.StagedContents)
	fmt.Printf("documents:    %d with a recorded staged file\n", report.StagedRoots)

	if len(report.Candidates) == 0 {
		fmt.Printf("\nEvery document's content is still staged. Nothing to reconcile.\n")
		return
	}

	fmt.Printf("\n%d document(s) have no staged file left", len(report.Candidates))
	if report.DescendantsTotal > 0 {
		fmt.Printf(", removing %d extracted descendant(s) with them", report.DescendantsTotal)
	}
	fmt.Printf(":\n\n")
	for _, candidate := range report.Candidates {
		fmt.Printf("  %s\n", candidate.SourcePath)
		fmt.Printf("      %s  doc %s", candidate.DocType, candidate.DocID[:8])
		if candidate.Descendants > 0 {
			fmt.Printf("  (+%d extracted)", candidate.Descendants)
		}
		fmt.Println()
	}
}
