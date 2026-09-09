package source

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"github.com/suankan/pocket-advisor/internal/domain"
	"os"
	"path/filepath"
	"testing"
)

type records map[string]Record

func (s records) SourceRecord(_ context.Context, id string) (Record, error) {
	r, ok := s[id]
	if !ok {
		return r, ErrUnavailable
	}
	return r, nil
}
func fixture(t *testing.T) (*Resolver, records, string) {
	t.Helper()
	dir := t.TempDir()
	store := records{}
	r, err := New([]string{dir}, func(p string) bool { return p == "ignored.txt" }, store)
	if err != nil {
		t.Fatal(err)
	}
	return r, store, dir
}
func put(t *testing.T, r *Resolver, s records, dir, name string, data []byte) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
		t.Fatal(err)
	}
	id := domain.NewDocID()
	s[id] = Record{DocID: id, SHA256: domain.SHA256Hex(data), Filename: name, Files: []File{{RootKey: r.RootKeys()[0], RelativePath: name}}}
	return id
}
func TestSourceRejectsChangedBytesWithSameAttributes(t *testing.T) {
	r, s, dir := fixture(t)
	id := put(t, r, s, dir, "doc.txt", []byte("before"))
	st, _ := os.Stat(filepath.Join(dir, "doc.txt"))
	if err := os.WriteFile(filepath.Join(dir, "doc.txt"), []byte("after!"), 0600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(filepath.Join(dir, "doc.txt"), st.ModTime(), st.ModTime())
	if _, err := r.Read(context.Background(), Ref(id), s[id].SHA256); !errors.Is(err, ErrChanged) {
		t.Fatalf("changed bytes accepted: %v", err)
	}
}
func TestSourceConfinementAndMissingRoots(t *testing.T) {
	r, s, dir := fixture(t)
	id := put(t, r, s, dir, "doc.txt", []byte("safe"))
	outside := filepath.Join(t.TempDir(), "secret.txt")
	_ = os.WriteFile(outside, []byte("secret"), 0600)
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../secret.txt", outside, "link", "ignored.txt", "x\\y"} {
		t.Run(p, func(t *testing.T) {
			_, err := r.ReadFile(context.Background(), File{RootKey: r.RootKeys()[0], RelativePath: p})
			if !errors.Is(err, ErrUnsafe) {
				t.Fatalf("unsafe path error: %v", err)
			}
		})
	}
	other, _, _ := fixture(t)
	other.Store = s
	if _, err := other.Read(context.Background(), Ref(id), ""); err == nil {
		t.Fatal("cross-workspace root accepted")
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := r.Walk(context.Background(), func(File, []byte) error { return nil }); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing mount: %v", err)
	}
}
func TestSourceReplayRepeatedNamesAndAlternativeParents(t *testing.T) {
	r, s, dir := fixture(t)
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for _, v := range []string{"first", "second"} {
		w, _ := z.Create("same.txt")
		_, _ = w.Write([]byte(v))
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	parent := put(t, r, s, dir, "bundle.zip", b.Bytes())
	rec := s[parent]
	rec.MimeType = "application/zip"
	s[parent] = rec
	children, err := Children(b.Bytes(), rec.MimeType, rec.Filename)
	if err != nil {
		t.Fatal(err)
	}
	if children[0].Selector == children[1].Selector {
		t.Fatal("ambiguous selector")
	}
	missing := domain.NewDocID()
	s[missing] = Record{DocID: missing, SHA256: rec.SHA256, Filename: rec.Filename, MimeType: rec.MimeType}
	child := domain.NewDocID()
	s[child] = Record{DocID: child, SHA256: domain.SHA256Hex(children[1].Data), Members: []Member{{ParentID: missing, Selector: children[1].Selector}, {ParentID: parent, Selector: children[1].Selector}}}
	got, err := r.Read(context.Background(), Ref(child), s[child].SHA256)
	if err != nil || string(got) != "second" {
		t.Fatalf("replay=%q %v", got, err)
	}
	budget := int64(len(b.Bytes()) + len(children[1].Data))
	if _, err := r.read(context.Background(), child, "", map[string]bool{}, 0, &budget); err == nil {
		t.Fatal("aggregate replay limit ignored siblings")
	}
}
func TestSourceWalkRenameAndIgnore(t *testing.T) {
	r, s, dir := fixture(t)
	id := put(t, r, s, dir, "before.txt", []byte("unchanged"))
	_ = os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("excluded"), 0600)
	if err := os.Rename(filepath.Join(dir, "before.txt"), filepath.Join(dir, "after.txt")); err != nil {
		t.Fatal(err)
	}
	n := 0
	err := r.Walk(context.Background(), func(f File, data []byte) error {
		n++
		if f.RelativePath != "after.txt" || f.SHA256 != s[id].SHA256 {
			t.Fatalf("rename changed identity: %+v", f)
		}
		return nil
	})
	if err != nil || n != 1 {
		t.Fatalf("scan %d %v", n, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Walk(ctx, func(File, []byte) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}
