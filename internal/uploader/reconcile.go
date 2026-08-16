package uploader

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"sort"

	"github.com/suankan/pocket-advisor/internal/storage/postgres"
)

// Deletion reconciliation answers one question: which documents came from a
// staged file that is no longer there?
//
// It deliberately inverts the pipeline's usual direction. Tier 1 is the
// authoritative store and the workspace directory is only a staging feed
// (ingestion-design.md §1), so a file disappearing from staging normally means
// nothing at all. This is the one operator-driven exception, and it stays an
// exception: nothing here runs during ingest, on a timer, or as a side effect
// of another mode. It reports by default and deletes only when the operator
// says so.

// ErrStagingEmpty refuses the whole operation when the staging directory
// yielded no files.
//
// An absent, unmounted, or half-synced directory is indistinguishable from a
// deliberately emptied one, and the difference is the entire corpus. Refusing
// is the only safe reading: a genuinely emptied workspace is served by
// --delete-data, which says what it does.
var ErrStagingEmpty = fmt.Errorf("staging directory contains no files: refusing to treat this as a request to delete every document")

// DeletionCandidate is one root document whose content is staged nowhere.
type DeletionCandidate struct {
	DocID       string
	SHA256      string
	SourcePath  string
	DocType     string
	Descendants int
}

// DeletionReport is the outcome of one reconciliation pass. It carries counts
// and candidates only, never file contents.
type DeletionReport struct {
	StagedFiles      int                 `json:"staged_files"`
	StagedContents   int                 `json:"staged_contents"`
	StagedRoots      int                 `json:"staged_roots"`
	Candidates       []DeletionCandidate `json:"candidates"`
	DescendantsTotal int                 `json:"descendants_total"`
	DryRun           bool                `json:"dry_run"`
	Deleted          int                 `json:"deleted"`
}

// Reconciler compares the staging directory against recorded provenance.
type Reconciler struct {
	docs  *postgres.DocumentRepo
	reset *Resetter
	log   *slog.Logger
}

func NewReconciler(docs *postgres.DocumentRepo, reset *Resetter, log *slog.Logger) *Reconciler {
	return &Reconciler{docs: docs, reset: reset, log: log}
}

// stagedContents hashes every file under root and returns the set of content
// hashes present.
//
// Identity is content, never path. Roughly half of a real staging directory
// can be the same bytes under two names, and documents are deduplicated on
// raw_sha256, so a document records only the first path its content was seen
// at. Judging by path would call a document deleted while its bytes are still
// on disk under the other name — and the next ingest would re-upload them
// under a fresh doc_id, silently invalidating anything that referenced the
// old one. Hashing also makes the comparison immune to filename
// normalization, which differs between what a filesystem stores and what was
// recorded.
func stagedContents(root string) (map[string]struct{}, int, error) {
	contents := make(map[string]struct{})
	files := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		sum, _, err := hashFile(path)
		if err != nil {
			return err
		}
		files++
		contents[sum] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, 0, fmt.Errorf("walk staging directory: %w", err)
	}
	return contents, files, nil
}

// Plan reports which documents have no staged file left. It writes nothing.
func (r *Reconciler) Plan(ctx context.Context, absPath string) (*DeletionReport, error) {
	contents, files, err := stagedContents(absPath)
	if err != nil {
		return nil, err
	}
	if files == 0 {
		return nil, ErrStagingEmpty
	}

	roots, err := r.docs.StagedRoots(ctx)
	if err != nil {
		return nil, err
	}

	report := &DeletionReport{
		StagedFiles:    files,
		StagedContents: len(contents),
		StagedRoots:    len(roots),
		DryRun:         true,
	}
	for _, root := range roots {
		if _, present := contents[root.SHA256]; present {
			continue
		}
		report.Candidates = append(report.Candidates, DeletionCandidate{
			DocID:       root.DocID,
			SHA256:      root.SHA256,
			SourcePath:  root.SourcePath,
			DocType:     root.DocType,
			Descendants: root.Descendants,
		})
		report.DescendantsTotal += root.Descendants
	}
	sort.Slice(report.Candidates, func(i, j int) bool {
		return report.Candidates[i].SourcePath < report.Candidates[j].SourcePath
	})
	return report, nil
}

// Apply deletes the planned candidates through the same content-hash path
// --forget uses, so removal stays idempotent and rerunnable after a partial
// failure rather than becoming a second deletion mechanism.
func (r *Reconciler) Apply(ctx context.Context, workspaceID string, report *DeletionReport) error {
	for _, candidate := range report.Candidates {
		if err := r.reset.Forget(ctx, workspaceID, candidate.SHA256); err != nil {
			return fmt.Errorf("delete %s: %w", candidate.DocID, err)
		}
		report.Deleted++
	}
	report.DryRun = false
	r.log.Info("deletion reconciliation applied",
		"workspace_id", workspaceID,
		"documents_deleted", report.Deleted,
		"descendants_removed", report.DescendantsTotal)
	return nil
}
