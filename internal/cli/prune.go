package cli

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/suankan/pocket-advisor/internal/app"
	"github.com/suankan/pocket-advisor/internal/retention"
	"github.com/suankan/pocket-advisor/internal/worker"
)

// ensureContainment brings the placement table up to date before any sweep
// or forget reads it.
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
	if report.Unexpandable > 0 {
		// Not a warning about this sweep: recorded containment holds these.
		// Said out loud anyway, because it is also the count of containers
		// whose text was never extractable, and that is worth knowing.
		fmt.Printf("unparsed: %d container(s) could not be re-read into members; "+
			"recorded containment covers them (see the retention log)\n",
			report.Unexpandable)
	}
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
