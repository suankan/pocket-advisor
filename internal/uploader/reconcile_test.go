package uploader

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// An empty staging directory must never be read as "delete everything". The
// reconciler is constructed with no stores at all: if the guard ever stops
// short-circuiting, this test panics on the nil repository instead of quietly
// planning a full-corpus deletion.
func TestPlanRefusesEmptyStagingDirectory(t *testing.T) {
	empty := t.TempDir()

	_, err := NewReconciler(nil, nil, quietLogger()).Plan(context.Background(), empty)
	if !errors.Is(err, ErrStagingEmpty) {
		t.Fatalf("expected ErrStagingEmpty for an empty directory, got %v", err)
	}
}

// A directory holding only subdirectories is still empty of files, and must be
// refused for the same reason.
func TestPlanRefusesDirectoryWithNoRegularFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "nested", "deeper"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := NewReconciler(nil, nil, quietLogger()).Plan(context.Background(), root)
	if !errors.Is(err, ErrStagingEmpty) {
		t.Fatalf("expected ErrStagingEmpty, got %v", err)
	}
}

// A missing directory is an error, never an empty walk.
func TestPlanFailsOnMissingDirectory(t *testing.T) {
	_, err := NewReconciler(nil, nil, quietLogger()).Plan(
		context.Background(), filepath.Join(t.TempDir(), "absent"))
	if err == nil {
		t.Fatal("expected an error for a missing staging directory")
	}
	if errors.Is(err, ErrStagingEmpty) {
		t.Fatal("a missing directory must not be reported as an empty one")
	}
}

// Identity is content, not path. The same bytes staged under two names are one
// content, and a document deduplicated onto that content must stay reachable
// through either name — deleting one copy cannot make it look absent.
func TestStagedContentsIsContentAddressedNotPathAddressed(t *testing.T) {
	root := t.TempDir()
	body := []byte("certificate of compliance")
	for _, name := range []string{"original.txt", "copy-under-another-name.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A genuinely different document alongside them.
	if err := os.WriteFile(filepath.Join(root, "other.txt"), []byte("unrelated"), 0o644); err != nil {
		t.Fatal(err)
	}

	contents, files, err := stagedContents(root)
	if err != nil {
		t.Fatal(err)
	}
	if files != 3 {
		t.Fatalf("expected 3 staged files, got %d", files)
	}
	if len(contents) != 2 {
		t.Fatalf("expected 2 distinct contents, got %d", len(contents))
	}

	// Removing one of the two identical files leaves the content staged.
	if err := os.Remove(filepath.Join(root, "original.txt")); err != nil {
		t.Fatal(err)
	}
	after, files, err := stagedContents(root)
	if err != nil {
		t.Fatal(err)
	}
	if files != 2 {
		t.Fatalf("expected 2 staged files after removal, got %d", files)
	}
	if len(after) != 2 {
		t.Fatalf("content identity changed when a duplicate name was removed: got %d", len(after))
	}
	for sum := range contents {
		if _, present := after[sum]; !present {
			t.Fatal("a content hash disappeared although its bytes are still staged")
		}
	}
}

// Nested files are staged files: the walk must not stop at the top level, or
// everything in a subdirectory would look deleted.
func TestStagedContentsWalksNestedDirectories(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "deep.txt"), []byte("deep"), 0o644); err != nil {
		t.Fatal(err)
	}

	contents, files, err := stagedContents(root)
	if err != nil {
		t.Fatal(err)
	}
	if files != 1 || len(contents) != 1 {
		t.Fatalf("expected the nested file to be staged, got files=%d contents=%d", files, len(contents))
	}
}
