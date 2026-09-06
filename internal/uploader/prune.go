package uploader

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/suankan/pocket-advisor/internal/storage/postgres"
)

// Pruning removes documents that nothing stages any more.
//
// It answers one question — is this document still reachable from a file the
// workspace stages? — and it answers it for the whole graph at once. A
// document is reachable when its own content is staged, or when something
// reachable contains it. Everything else is residue: a file deleted from the
// staging directory, a file an ignore pattern now excludes, an attachment
// whose only carrier was one of those.
//
// It replaces reasoning about direct references, which cannot be made correct
// here. Containment nests, so removing one container orphans a subtree rather
// than a row, and content identity is shared, so the same attachment can be
// held by many messages at once. Reachability handles both without a special
// case, and terminates on its own if lineage were ever cyclic.
//
// Two guards sit in front of it, because this is the one part of ingestion
// that deletes. ErrPlacementsIncomplete refuses to sweep against containment
// data that is known to be under-counted, and ErrBlastRadius refuses a sweep
// whose size says the staging directory is missing rather than changed.

// ErrPlacementsIncomplete stops a sweep before the containment backfill has
// re-derived what ingest originally discarded.
//
// Until then document_placements holds only what parent_doc_id could seed —
// one container per document — so an attachment carried by three emails looks
// held by one. Sweeping on that deletes the other two emails' copies, which is
// the exact loss the placements table exists to prevent, and nothing afterwards
// records that it happened.
var ErrPlacementsIncomplete = errors.New(
	"containment backfill has not completed: pruning would treat shared attachments as unheld")

// ErrBlastRadius stops a sweep that would remove more of the corpus than a
// change to the staging directory plausibly explains.
//
// An unmounted volume, a half-finished mail sync and a deliberately emptied
// folder are indistinguishable from here — all three simply present fewer
// files — and only one of them means the documents should go. ErrStagingEmpty
// already refuses the total case; this refuses the shape of it, where a
// directory is present but most of it is not.
var ErrBlastRadius = errors.New(
	"pruning would remove an implausible share of the corpus: refusing in case staging is incomplete")

// DefaultBlastRadius is the share of a workspace's documents a single sweep
// may remove before it refuses without an explicit override. A tenth is far
// above any ordinary edit to a staging directory and far below the shape of a
// directory that failed to mount.
const DefaultBlastRadius = 0.10

// PruneReport is the outcome of one sweep. Counts and identifiers only; a
// document's text never appears here.
type PruneReport struct {
	// Documents the sweep found unreachable, in reporting order.
	Candidates []postgres.PruneCandidate `json:"candidates"`
	// StagedContents is how many distinct contents the staging directories
	// hold, and TotalDocuments how many the workspace holds. Together they are
	// what the blast-radius guard judged.
	StagedContents int  `json:"staged_contents"`
	StagedFiles    int  `json:"staged_files"`
	TotalDocuments int  `json:"total_documents"`
	Deleted        int  `json:"deleted"`
	DryRun         bool `json:"dry_run"`
	// Roots and Contained split the candidates by whether they were staged in
	// their own right, which is the difference between "you deleted a file"
	// and "the only thing carrying this went away".
	Roots     int `json:"root_candidates"`
	Contained int `json:"contained_candidates"`
}

// PruneStore is the database half of a sweep.
type PruneStore interface {
	PlacementsBackfilledAt(ctx context.Context) (time.Time, error)
	UnreachableDocuments(ctx context.Context, stagedSHAs []string) ([]postgres.PruneCandidate, error)
	CountDocuments(ctx context.Context) (int, error)
	DeleteDocuments(ctx context.Context, ids []string) (int64, error)
}

// Pruner sweeps one workspace.
type Pruner struct {
	docs  PruneStore
	reset interface{ DrainPending(context.Context) error }
	log   *slog.Logger
}

func NewPruner(docs PruneStore, reset interface{ DrainPending(context.Context) error }, log *slog.Logger) *Pruner {
	return &Pruner{docs: docs, reset: reset, log: log}
}

// PruneOptions bounds one sweep.
type PruneOptions struct {
	// AbsPaths and Ignore describe what the workspace currently stages, and
	// must be the same pair the uploader walked: a sweep judging a wider or
	// narrower set of files than the upload did would delete what was just
	// added, or keep what was just excluded.
	AbsPaths []string
	Ignore   func(rel string) bool
	// MaxShare overrides DefaultBlastRadius. A share of 1 or more disables the
	// guard, which is only ever right when an operator has looked at the plan.
	MaxShare float64
}

// Plan reports what a sweep would remove, and writes nothing.
func (p *Pruner) Plan(ctx context.Context, o PruneOptions) (*PruneReport, error) {
	at, err := p.docs.PlacementsBackfilledAt(ctx)
	if err != nil {
		return nil, err
	}
	if at.IsZero() {
		return nil, ErrPlacementsIncomplete
	}

	contents, files, err := stagedContents(o.AbsPaths, o.Ignore)
	if err != nil {
		return nil, err
	}
	if files == 0 {
		return nil, ErrStagingEmpty
	}

	shas := make([]string, 0, len(contents))
	for sha := range contents {
		shas = append(shas, sha)
	}

	candidates, err := p.docs.UnreachableDocuments(ctx, shas)
	if err != nil {
		return nil, err
	}
	total, err := p.docs.CountDocuments(ctx)
	if err != nil {
		return nil, err
	}

	report := &PruneReport{
		Candidates:     candidates,
		StagedContents: len(contents),
		StagedFiles:    files,
		TotalDocuments: total,
		DryRun:         true,
	}
	for _, c := range candidates {
		if c.SourcePath != "" {
			report.Roots++
		} else {
			report.Contained++
		}
	}

	if err := report.withinBlastRadius(o.MaxShare); err != nil {
		return report, err
	}
	return report, nil
}

// withinBlastRadius judges the sweep's size against the corpus it would cut.
func (r *PruneReport) withinBlastRadius(maxShare float64) error {
	if maxShare <= 0 {
		maxShare = DefaultBlastRadius
	}
	if maxShare >= 1 || r.TotalDocuments == 0 || len(r.Candidates) == 0 {
		return nil
	}
	share := float64(len(r.Candidates)) / float64(r.TotalDocuments)
	if share <= maxShare {
		return nil
	}
	return fmt.Errorf("%w: %d of %d documents (%.1f%%, limit %.1f%%)",
		ErrBlastRadius, len(r.Candidates), r.TotalDocuments, share*100, maxShare*100)
}

// Apply deletes the planned candidates.
//
// Tier 2 removal and durable object cleanup requests commit together. A failed
// cleanup fails the run; its requests remain available without document rows.
func (p *Pruner) Apply(ctx context.Context, workspaceID string, report *PruneReport) error {
	if len(report.Candidates) == 0 {
		report.DryRun = false
		return nil
	}

	ids := make([]string, 0, len(report.Candidates))
	for _, c := range report.Candidates {
		ids = append(ids, c.DocID)
	}
	deleted, err := p.docs.DeleteDocuments(ctx, ids)
	if err != nil {
		return err
	}
	report.Deleted = int(deleted)

	if err := p.reset.DrainPending(ctx); err != nil {
		return err
	}

	report.DryRun = false
	p.log.Info("prune applied",
		"workspace_id", workspaceID,
		"documents_deleted", report.Deleted,
		"root_candidates", report.Roots,
		"contained_candidates", report.Contained)
	return nil
}
