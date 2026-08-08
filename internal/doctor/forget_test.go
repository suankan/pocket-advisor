package doctor

import (
	"context"
	"testing"
)

type fakeCollector struct {
	descendants []string
	rawURIs     []string
	extractedURIs []string
}

func (f *fakeCollector) Descendants(_ context.Context, root string) ([]string, error) {
	return f.descendants, nil
}

func (f *fakeCollector) Tier1URIs(_ context.Context, ids []string) (raw, extracted []string, err error) {
	return f.rawURIs, f.extractedURIs, nil
}

type fakeDeleter struct {
	deletedIDs    []string
	deletedObjects []string
	deleteErr     error
}

func (f *fakeDeleter) DeleteDocIDs(_ context.Context, ids []string) (int64, error) {
	if f.deleteErr != nil {
		return 0, f.deleteErr
	}
	f.deletedIDs = append(f.deletedIDs, ids...)
	return int64(len(ids)), nil
}

func (f *fakeDeleter) DeleteObjects(_ context.Context, keys []string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deletedObjects = append(f.deletedObjects, keys...)
	return nil
}

func TestCollectForgetTarget(t *testing.T) {
	c := &fakeCollector{
		descendants:   []string{"root", "child1", "child2"},
		rawURIs:       []string{"raw/ab/abcdef"},
		extractedURIs: []string{"extracted/cd/cdef01"},
	}
	target, err := CollectForgetTarget(context.Background(), c, "root")
	if err != nil {
		t.Fatal(err)
	}
	if len(target.DocIDs) != 3 {
		t.Errorf("expected 3 descendants, got %d", len(target.DocIDs))
	}
	if len(target.RawURIs) != 1 {
		t.Errorf("expected 1 raw URI, got %d", len(target.RawURIs))
	}
	if len(target.ExtractURIs) != 1 {
		t.Errorf("expected 1 extracted URI, got %d", len(target.ExtractURIs))
	}
}

func TestExecuteForget(t *testing.T) {
	del := &fakeDeleter{}
	target := &ForgetTarget{
		DocIDs:      []string{"d1", "d2"},
		RawURIs:     []string{"raw/ab/abc"},
		ExtractURIs: []string{"extracted/cd/cde"},
	}
	if err := ExecuteForget(context.Background(), del, target, nil); err != nil {
		t.Fatal(err)
	}
	if len(del.deletedIDs) != 2 {
		t.Errorf("expected 2 IDs deleted, got %d", len(del.deletedIDs))
	}
	if len(del.deletedObjects) != 2 {
		t.Errorf("expected 2 objects deleted, got %d", len(del.deletedObjects))
	}
}

func TestExecuteForgetIdempotent(t *testing.T) {
	del := &fakeDeleter{}
	target := &ForgetTarget{DocIDs: nil, RawURIs: nil, ExtractURIs: nil}
	if err := ExecuteForget(context.Background(), del, target, nil); err != nil {
		t.Fatal(err)
	}
	if len(del.deletedIDs) != 0 {
		t.Error("expected no deletions for empty target")
	}
}
