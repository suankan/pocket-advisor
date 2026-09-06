package uploader

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/suankan/pocket-advisor/internal/storage/postgres"
)

type pendingStore struct {
	shas        []string
	completeErr error
}

func (s *pendingStore) PendingObjectDeletions(context.Context, int) ([]string, error) {
	return append([]string(nil), s.shas...), nil
}
func (s *pendingStore) CompleteObjectDeletion(_ context.Context, sha string) error {
	if s.completeErr != nil {
		return s.completeErr
	}
	for i, v := range s.shas {
		if v == sha {
			s.shas = append(s.shas[:i], s.shas[i+1:]...)
			break
		}
	}
	return nil
}

func TestObjectDeletionResumesWithoutDocumentRows(t *testing.T) {
	ctx := context.Background()
	s := &pendingStore{shas: []string{"first", "second", "third"}}
	var removed []string
	failure := errors.New("object store unavailable")
	err := drainObjectDeletions(ctx, s, func(_ context.Context, sha string) error {
		if sha == "second" {
			return failure
		}
		removed = append(removed, sha)
		return nil
	})
	if !errors.Is(err, failure) {
		t.Fatalf("error = %v", err)
	}
	if !reflect.DeepEqual(s.shas, []string{"second", "third"}) {
		t.Fatalf("lost cleanup work: %v", s.shas)
	}
	if err := drainObjectDeletions(ctx, s, func(_ context.Context, sha string) error {
		removed = append(removed, sha)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(removed, []string{"first", "second", "third"}) || len(s.shas) != 0 {
		t.Fatalf("removed=%v pending=%v", removed, s.shas)
	}
}

func TestObjectDeletionCompletionFailureKeepsRequest(t *testing.T) {
	s := &pendingStore{shas: []string{"content"}, completeErr: errors.New("database unavailable")}
	if err := drainObjectDeletions(context.Background(), s, func(context.Context, string) error { return nil }); err == nil {
		t.Fatal("completion failure was hidden")
	}
	if len(s.shas) != 1 {
		t.Fatal("request lost after completion failure")
	}
}

type failedCleanup struct{ err error }

func (f failedCleanup) DrainPending(context.Context) error { return f.err }

func TestPruneFailsWhenCommittedObjectCleanupFails(t *testing.T) {
	failure := errors.New("cleanup failed")
	store := &fakePruneStore{}
	p := NewPruner(store, failedCleanup{failure}, quietLogger())
	report := &PruneReport{DryRun: true, Candidates: []postgres.PruneCandidate{{DocID: "document"}}}
	if err := p.Apply(context.Background(), "synthetic", report); !errors.Is(err, failure) {
		t.Fatalf("Apply error = %v", err)
	}
	if report.Deleted != 1 {
		t.Fatalf("committed deletion count = %d", report.Deleted)
	}
}
