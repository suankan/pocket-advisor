package retention

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/suankan/pocket-advisor/internal/storage/postgres"
)

// fakePruneStore stands in for the workspace database. The reachability query
// itself is SQL and is exercised against a live database in the manual suite;
// what is hermetic here is everything guarding it.
type fakePruneStore struct {
	backfilledAt time.Time
	unreachable  []postgres.PruneCandidate
	total        int
	deletedIDs   []string
	deleteErr    error
}

func (f *fakePruneStore) PlacementsBackfilledAt(context.Context) (time.Time, error) {
	return f.backfilledAt, nil
}
func (f *fakePruneStore) UnreachableDocuments(context.Context, []string, []string) ([]postgres.PruneCandidate, error) {
	return f.unreachable, nil
}
func (f *fakePruneStore) CountDocuments(context.Context) (int, error) { return f.total, nil }
func (f *fakePruneStore) DeleteDocuments(_ context.Context, ids []string) (int64, error) {
	if f.deleteErr != nil {
		return 0, f.deleteErr
	}
	f.deletedIDs = append(f.deletedIDs, ids...)
	return int64(len(ids)), nil
}

func stagedDir(t *testing.T, files ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("content of "+f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func backfilled() time.Time { return time.Now().Add(-time.Hour) }

// Sweeping before the containment backfill reads a placement table that holds
// only what parent_doc_id could seed — one container per document — so a
// shared attachment looks held by one email and is deleted along with it. The
// refusal is the whole reason the marker exists.
func TestPlanRefusesUntilContainmentIsBackfilled(t *testing.T) {
	store := &fakePruneStore{total: 10} // zero backfilledAt
	p := NewPruner(store, quietLogger())

	_, err := p.Plan(context.Background(), PruneOptions{
		AbsPaths: []string{stagedDir(t, "a.eml")},
	})
	if !errors.Is(err, ErrPlacementsIncomplete) {
		t.Fatalf("expected ErrPlacementsIncomplete, got %v", err)
	}
}

// An empty staging directory must never be read as "delete everything": an
// unmounted volume presents exactly the same way.
func TestPlanRefusesEmptyStaging(t *testing.T) {
	store := &fakePruneStore{backfilledAt: backfilled(), total: 10}
	p := NewPruner(store, quietLogger())

	_, err := p.Plan(context.Background(), PruneOptions{AbsPaths: []string{t.TempDir()}})
	if !errors.Is(err, ErrStagingEmpty) {
		t.Fatalf("expected ErrStagingEmpty, got %v", err)
	}
}

// A directory holding only subdirectories is still empty of files.
func TestPlanRefusesDirectoryWithNoRegularFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "nested", "deeper"), 0o755); err != nil {
		t.Fatal(err)
	}
	store := &fakePruneStore{backfilledAt: backfilled(), total: 10}
	p := NewPruner(store, quietLogger())

	_, err := p.Plan(context.Background(), PruneOptions{AbsPaths: []string{root}})
	if !errors.Is(err, ErrStagingEmpty) {
		t.Fatalf("expected ErrStagingEmpty, got %v", err)
	}
}

// A missing directory is an error, never an empty walk.
func TestPlanFailsOnMissingDirectory(t *testing.T) {
	store := &fakePruneStore{backfilledAt: backfilled(), total: 10}
	p := NewPruner(store, quietLogger())

	_, err := p.Plan(context.Background(), PruneOptions{
		AbsPaths: []string{filepath.Join(t.TempDir(), "absent")},
	})
	if err == nil {
		t.Fatal("expected an error for a missing staging directory")
	}
	if errors.Is(err, ErrStagingEmpty) {
		t.Fatal("a missing directory must not be reported as an empty one")
	}
}

// Splitting the report by whether a candidate was staged in its own right is
// what tells an operator "you deleted this file" apart from "the only thing
// carrying this went away".
func TestPlanSplitsRootsFromContained(t *testing.T) {
	store := &fakePruneStore{
		backfilledAt: backfilled(),
		total:        100,
		unreachable: []postgres.PruneCandidate{
			{DocID: "1", SourcePath: "mail/INBOX.msf"},
			{DocID: "2", SourceName: "attachment.pdf"},
			{DocID: "3", SourceName: "another.pdf"},
		},
	}
	p := NewPruner(store, quietLogger())

	report, err := p.Plan(context.Background(), PruneOptions{
		AbsPaths: []string{stagedDir(t, "a.eml")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Roots != 1 || report.Contained != 2 {
		t.Errorf("roots=%d contained=%d, want 1 and 2", report.Roots, report.Contained)
	}
}

// Nothing to remove must not be reported as a deletion, and must not leave the
// report claiming it was still only a plan.
func TestApplyWithNoCandidatesDeletesNothing(t *testing.T) {
	store := &fakePruneStore{backfilledAt: backfilled(), total: 10}
	p := NewPruner(store, quietLogger())

	report := &PruneReport{DryRun: true}
	if err := p.Apply(context.Background(), "ws", report); err != nil {
		t.Fatal(err)
	}
	if len(store.deletedIDs) != 0 {
		t.Errorf("deleted %v with no candidates", store.deletedIDs)
	}
	if report.DryRun {
		t.Error("an applied report still claims to be a dry run")
	}
}
