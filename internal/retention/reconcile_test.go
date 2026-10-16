package retention

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/suankan/pocket-advisor/internal/domain"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
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

	st, err := stagedContents([]string{root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.files != 3 {
		t.Fatalf("expected 3 staged files, got %d", st.files)
	}
	if len(st.contents) != 2 {
		t.Fatalf("expected 2 distinct contents, got %d", len(st.contents))
	}

	// Removing one of the two identical files leaves the content staged.
	if err := os.Remove(filepath.Join(root, "original.txt")); err != nil {
		t.Fatal(err)
	}
	after, err := stagedContents([]string{root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if after.files != 2 {
		t.Fatalf("expected 2 staged files after removal, got %d", after.files)
	}
	if len(after.contents) != 2 {
		t.Fatalf("content identity changed when a duplicate name was removed: got %d", len(after.contents))
	}
	for sum := range st.contents {
		if _, present := after.contents[sum]; !present {
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

	st, err := stagedContents([]string{root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.files != 1 || len(st.contents) != 1 {
		t.Fatalf("expected the nested file to be staged, got files=%d contents=%d", st.files, len(st.contents))
	}
}

// A container the parser cannot open is not a reason to refuse the corpus.
//
// Real mail carries a steady fraction of these — a multipart whose part begins
// with raw HTML instead of headers, an unknown charset, a corrupt archive — and
// aborting the traversal on the first one meant `--ingest-all` could not run at
// all. Retention reaches members through recorded containment, so an unopenable
// container costs a count, not the sweep (ingestion-design.md §6).
func TestStagedContentsCountsUnopenableContainersAndKeepsWalking(t *testing.T) {
	root := t.TempDir()

	// Exactly the shape that stopped a real run: a declared boundary followed
	// by a body that never supplies the part's MIME headers.
	broken := "From: sender@example.test\r\n" +
		"To: recipient@example.test\r\n" +
		"Subject: Broken multipart\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/alternative; boundary=\"BOUND\"\r\n\r\n" +
		"--BOUND\r\n" +
		"<!DOCTYPE HTML PUBLIC \"-//W3C//DTD HTML 4.0 Transitional//EN\">\r\n" +
		"<html><body>no part headers at all</body></html>\r\n" +
		"--BOUND--\r\n"

	attachment := "attached evidence"
	good := "From: sender@example.test\r\n" +
		"To: recipient@example.test\r\n" +
		"Subject: Well formed\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"OK\"\r\n\r\n" +
		"--OK\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"Body prose.\r\n" +
		"--OK\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Disposition: attachment; filename=\"evidence.txt\"\r\n\r\n" +
		attachment + "\r\n" +
		"--OK--\r\n"

	for name, body := range map[string]string{"broken.eml": broken, "good.eml": good} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	st, err := stagedContents([]string{root}, nil)
	if err != nil {
		t.Fatalf("one unopenable container ended the whole traversal: %v", err)
	}
	if st.files != 2 {
		t.Fatalf("walked %d files, want both", st.files)
	}
	if st.unexpandable != 1 {
		t.Fatalf("counted %d unopenable containers, want exactly the broken one", st.unexpandable)
	}
	// The broken message is still staged content in its own right: it is a
	// document, and a sweep must not treat it as absent.
	if _, ok := st.contents[domain.SHA256Hex([]byte(broken))]; !ok {
		t.Error("the unopenable container's own content is missing from the staged set")
	}
	// And the walk carried on far enough to open the message that was fine.
	if _, ok := st.contents[domain.SHA256Hex([]byte(attachment))]; !ok {
		t.Error("a later container's members were skipped after an earlier failure")
	}
}

// A member the ignore rules exclude by name is not staged content: its bytes
// are recorded as ignored and nothing inside it is expanded, while its
// siblings and a copy staged under another name stay staged.
func TestStagedContentsLeavesOutIgnoredMembers(t *testing.T) {
	root := t.TempDir()
	book, letter := []byte("a synthetic book, chapter one"), []byte("a synthetic letter")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range map[string][]byte{"Book.FB2": book, "letter.txt": letter} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bundle.zip"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	isBook := func(name string) bool { return strings.HasSuffix(strings.ToLower(name), ".fb2") }

	st, err := stagedContentsContext(context.Background(), []string{root}, nil, isBook, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.contents[domain.SHA256Hex(letter)]; !ok {
		t.Error("the kept member is not staged")
	}
	if _, ok := st.contents[domain.SHA256Hex(book)]; ok {
		t.Error("the ignored member is staged")
	}
	if _, ok := st.ignored[domain.SHA256Hex(book)]; !ok {
		t.Error("the ignored member is not recorded as ignored")
	}

	// The same book also staged as a file of its own, under a kept name.
	if err := os.WriteFile(filepath.Join(root, "book.txt"), book, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err = stagedContentsContext(context.Background(), []string{root}, nil, isBook, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.contents[domain.SHA256Hex(book)]; !ok {
		t.Error("a copy staged under a kept name is not staged")
	}
}
