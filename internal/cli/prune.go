package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/suankan/pocket-advisor/internal/app"
	"github.com/suankan/pocket-advisor/internal/retention"
	"github.com/suankan/pocket-advisor/internal/telemetry"
	"github.com/suankan/pocket-advisor/internal/worker"
	"github.com/suankan/pocket-advisor/internal/workspace"
)

// prune removes unreachable documents before this process starts its pools.
// It requires complete containment and enforces a bounded deletion share.
func prune(ctx context.Context, o *Options, a *app.App) error {
	if o.DryRun {
		return nil
	}
	ws, err := workspace.Load(o.WorkspaceConfig, o.WorkspaceID)
	if err != nil {
		return err
	}
	log := a.Logger(telemetry.RoleRetention)

	if err := ensureContainment(ctx, a, log); err != nil {
		return err
	}

	reset := retention.NewResetter(a.Docs, a.Bus, log)
	pruner := retention.NewPruner(a.Docs, reset, log)

	report, err := pruner.Plan(ctx, retention.PruneOptions{
		AbsPaths: ws.AbsPaths,
		Ignore:   ws.Ignore.Match,
		MaxShare: o.PruneMaxShare,
	})
	switch {
	case errors.Is(err, retention.ErrBlastRadius):
		// Report the refusal and continue with the additive intake phase.
		renderPrunePlan(report, ws.ID)
		fmt.Printf("\n%v\n", err)
		fmt.Printf("Review the plan above. If it is correct, re-run with " +
			"--prune-max-share 1 to allow it.\n")
		return nil
	case err != nil:
		return err
	}

	if len(report.Candidates) == 0 {
		log.Info("prune: nothing to remove",
			"workspace_id", ws.ID, "documents", report.TotalDocuments)
		return nil
	}

	renderPrunePlan(report, ws.ID)
	if err := pruner.Apply(ctx, ws.ID, report); err != nil {
		return err
	}
	fmt.Printf("pruned:   %d document(s) removed\n", report.Deleted)
	return nil
}

// ensureContainment brings the placement table up to date before any sweep
// reads it.
//
// It runs at most once per workspace, and only because the alternative is
// worse: pruning against the parent_doc_id floor deletes attachments that
// other messages still carry, silently and unrecoverably. The cost is one
// pass over every container's original bytes, paid the first time a sweep would
// otherwise have been unsafe.
func ensureContainment(ctx context.Context, a *app.App, log *slog.Logger) error {
	if err := a.DB.ApplyPlacementSchema(ctx); err != nil {
		return err
	}
	at, err := a.Docs.PlacementsBackfilledAt(ctx)
	if err != nil {
		return err
	}
	if !at.IsZero() {
		return nil
	}

	// A caller that built its app without Tier 1 cannot re-derive anything, and
	// a nil vault would only announce itself as a panic several frames down.
	if a.Sources == nil {
		return fmt.Errorf("containment backfill needs original-source access: this command " +
			"was built without it, which is a wiring mistake rather than a workspace problem")
	}

	log.Info("containment backfill starting: re-deriving what each container " +
		"holds, so pruning can tell a shared attachment from an unheld one")
	b := &worker.PlacementBackfiller{
		Docs:    a.Docs,
		Write:   a.Docs,
		Sources: a.Sources,
		Log:     log,
	}
	s, err := b.Run(ctx, 0)
	if err != nil {
		return err
	}
	if !s.Complete {
		return fmt.Errorf(
			"containment backfill incomplete: %d container(s) unreadable and %d unparsable-but-known-to-have-children, "+
				"of %d containers (%d unparsable in total, the rest of which never produced children). "+
				"Pruning stays disabled: a container whose contents cannot be re-derived may be the only thing "+
				"holding an attachment, and sweeping without it would delete one that is still carried",
			s.Unreadable, s.UnparsableWithChildren, s.Containers, s.Unparsable)
	}
	log.Info("containment backfill complete",
		"containers", s.Containers, "placements", s.Placements,
		"unparsable_without_children", s.Unparsable)
	return nil
}

// renderPrunePlan prints what is about to be removed, or was refused. Paths
// and filenames are the operator's own, on the operator's own terminal, and
// never leave the process.
func renderPrunePlan(report *retention.PruneReport, workspaceID string) {
	if report == nil {
		return
	}
	fmt.Printf("\nprune:    workspace=%s\n", workspaceID)
	fmt.Printf("staged:   %d files, %d distinct contents\n",
		report.StagedFiles, report.StagedContents)
	fmt.Printf("corpus:   %d documents\n", report.TotalDocuments)
	fmt.Printf("unheld:   %d document(s) — %d staged in their own right, %d held by nothing else\n",
		len(report.Candidates), report.Roots, report.Contained)

	const show = 20
	for i, c := range report.Candidates {
		if i == show {
			fmt.Printf("    ... and %d more\n", len(report.Candidates)-show)
			break
		}
		name := c.SourcePath
		if name == "" {
			name = c.SourceName + "  (contained)"
		}
		fmt.Printf("    %-8s %s\n", c.DocType, name)
	}
}
