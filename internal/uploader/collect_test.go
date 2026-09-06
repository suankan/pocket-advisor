package uploader

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stage writes files under a fresh root and returns it.
func stage(t *testing.T, files ...string) string {
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

func rels(t *testing.T, root string, files []stagedFile) []string {
	t.Helper()
	var out []string
	for _, f := range files {
		rel, err := filepath.Rel(f.Root, f.Path)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, filepath.ToSlash(rel))
	}
	return out
}

// The walk is where an ignore pattern has to take effect: a file excluded here
// is never hashed, never uploaded, and never becomes a document.
func TestCollectSkipsIgnoredFiles(t *testing.T) {
	root := stage(t,
		"INBOX.msf",
		"INBOX/cur/a.eml",
		"INBOX/cur/b.eml",
		"[Gmail].sbd/Drafts.msf",
		"notes.txt",
	)

	ignore := func(rel string) bool { return strings.HasSuffix(rel, ".msf") }
	files, ignored, err := collect([]string{root}, ignore)
	if err != nil {
		t.Fatal(err)
	}
	if ignored != 2 {
		t.Errorf("ignored = %d, want the two .msf files", ignored)
	}

	got := strings.Join(rels(t, root, files), ",")
	for _, want := range []string{"INBOX/cur/a.eml", "INBOX/cur/b.eml", "notes.txt"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s was excluded but should have been staged; got %s", want, got)
		}
	}
	if strings.Contains(got, ".msf") {
		t.Errorf("an ignored file reached the upload set: %s", got)
	}
}

// A nil matcher is what a workspace configuring no patterns passes, and it must
// behave exactly as the walk did before the option existed.
func TestCollectWithoutAMatcherTakesEverything(t *testing.T) {
	root := stage(t, "a.eml", "deep/b.msf")

	files, ignored, err := collect([]string{root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ignored != 0 {
		t.Errorf("ignored = %d with no matcher, want 0", ignored)
	}
	if len(files) != 2 {
		t.Errorf("collected %d files, want both", len(files))
	}
}

// Patterns are written against a workspace directory's own shape, so what the
// matcher sees must be relative to the root, never the absolute host path.
func TestCollectMatchesRelativeToTheRoot(t *testing.T) {
	root := stage(t, "INBOX/cur/a.eml")

	var seen []string
	_, _, err := collect([]string{root}, func(rel string) bool {
		seen = append(seen, rel)
		return false
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "INBOX/cur/a.eml" {
		t.Errorf("matcher saw %v, want the root-relative path", seen)
	}
	for _, s := range seen {
		if filepath.IsAbs(s) {
			t.Errorf("matcher was handed an absolute path: %s", s)
		}
	}
}

// Each root is matched against on its own terms, so one workspace's several
// directories can hold the same relative shape without confusing a pattern.
func TestCollectAppliesIgnoreAcrossEveryRoot(t *testing.T) {
	a := stage(t, "keep.eml", "drop.msf")
	b := stage(t, "keep.eml", "drop.msf")

	files, ignored, err := collect([]string{a, b},
		func(rel string) bool { return strings.HasSuffix(rel, ".msf") })
	if err != nil {
		t.Fatal(err)
	}
	if ignored != 2 {
		t.Errorf("ignored = %d, want one per root", ignored)
	}
	if len(files) != 2 {
		t.Errorf("collected %d files, want one kept per root", len(files))
	}
}
