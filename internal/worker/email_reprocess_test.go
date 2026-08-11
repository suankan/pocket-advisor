package worker

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/engine/email"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/storage/rustfs"
)

// Fakes for the three collaborators a reprocessing run has: the selector that
// decides which documents are in scope, Tier 1, and the metadata write path.
// Every address here is a .test domain, which cannot resolve to a real
// mailbox, and no fixture is read from a workspace.

type fakeSelector struct {
	// docs is the workspace's email message documents in the deterministic
	// order the real query returns them: by doc_id.
	docs []domain.Document
	// missing is the subset with no metadata row yet.
	missing map[string]bool

	mu      sync.Mutex
	queries []postgres.EmailDocumentQuery
	err     error
}

func (f *fakeSelector) EmailDocuments(_ context.Context, q postgres.EmailDocumentQuery) ([]domain.Document, error) {
	f.mu.Lock()
	f.queries = append(f.queries, q)
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}

	var out []domain.Document
	for _, d := range f.docs {
		if d.WorkspaceID != q.WorkspaceID {
			continue
		}
		if q.After != "" && d.DocID <= q.After {
			continue
		}
		if q.OnlyMissing && !f.missing[d.DocID] {
			continue
		}
		out = append(out, d)
		if len(out) == q.Limit {
			break
		}
	}
	return out, nil
}

type fakeVault struct {
	objects map[string][]byte
	// unreadable simulates an object the store will not serve: present in
	// Tier 2, absent or erroring in Tier 1.
	unreadable map[string]bool
	mu         sync.Mutex
	inFlight   int
	peak       int
}

func (f *fakeVault) KeyFromURI(uri string) (string, error) {
	const prefix = "s3://bucket/"
	if !strings.HasPrefix(uri, prefix) {
		return "", fmt.Errorf("uri %q is not in bucket", uri)
	}
	return strings.TrimPrefix(uri, prefix), nil
}

func (f *fakeVault) Get(_ context.Context, key string) ([]byte, rustfs.Provenance, error) {
	f.mu.Lock()
	f.inFlight++
	if f.inFlight > f.peak {
		f.peak = f.inFlight
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()

	if f.unreadable[key] {
		return nil, rustfs.Provenance{}, errors.New("object store: read failed")
	}
	data, ok := f.objects[key]
	if !ok {
		return nil, rustfs.Provenance{}, errors.New("object store: 404")
	}
	return data, rustfs.Provenance{}, nil
}

type fakeWriter struct {
	mu      sync.Mutex
	saved   map[string]domain.EmailMessage
	order   []string
	calls   int32
	failFor map[string]bool
}

func (f *fakeWriter) SaveEmailMessage(_ context.Context, m domain.EmailMessage) (domain.EmailConversation, error) {
	atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failFor[m.DocID] {
		return domain.EmailConversation{}, errors.New("write failed")
	}
	if f.saved == nil {
		f.saved = map[string]domain.EmailMessage{}
	}
	if _, seen := f.saved[m.DocID]; !seen {
		f.order = append(f.order, m.DocID)
	}
	// Upsert, exactly like the repository: a second write of the same doc_id
	// replaces the row instead of adding one.
	f.saved[m.DocID] = m
	return domain.EmailConversation{ConversationID: "conv-" + m.MessageID}, nil
}

const testWorkspace = "ws-test"

func synthetic(messageID, subject, from string) []byte {
	return []byte("Message-ID: <" + messageID + ">\r\n" +
		"From: " + from + "\r\n" +
		"To: recipient@example.test\r\n" +
		"Subject: " + subject + "\r\n" +
		"Date: Tue, 07 Jan 2026 08:12:30 +0000\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"Synthetic body.\r\n")
}

func emailDoc(docID, key string) domain.Document {
	return domain.Document{
		DocID:       docID,
		WorkspaceID: testWorkspace,
		MimeType:    "message/rfc822",
		RawURI:      "s3://bucket/" + key,
	}
}

// fixture builds a reprocessor over three readable synthetic messages.
func fixture() (*EmailMetadataReprocessor, *fakeSelector, *fakeVault, *fakeWriter) {
	sel := &fakeSelector{missing: map[string]bool{}}
	vault := &fakeVault{objects: map[string][]byte{}, unreadable: map[string]bool{}}
	writer := &fakeWriter{failFor: map[string]bool{}}
	for _, id := range []string{"a", "b", "c"} {
		key := "raw/" + id
		vault.objects[key] = synthetic("msg-"+id+"@example.test", "Subject "+id, "sender@example.test")
		sel.docs = append(sel.docs, emailDoc("doc-"+id, key))
	}
	return &EmailMetadataReprocessor{
		Docs: sel, Vault: vault, Emails: writer, Log: quietLogger(),
	}, sel, vault, writer
}

// ---- selection --------------------------------------------------------------

// The walk is scoped to one workspace, ordered, and paged by doc_id: the
// cursor of the next query is the last document of the previous batch, so an
// interrupted run resumes over the same sequence rather than a reshuffled one.
func TestReprocessSelectsTheWorkspaceInDeterministicPages(t *testing.T) {
	r, sel, _, writer := fixture()
	sel.docs = append(sel.docs, domain.Document{
		DocID: "doc-other", WorkspaceID: "ws-elsewhere", RawURI: "s3://bucket/raw/other",
	})

	summary, err := r.Run(context.Background(), EmailReprocessOptions{
		WorkspaceID: testWorkspace, BatchSize: 2,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.Processed != 3 || summary.Updated != 3 {
		t.Fatalf("summary = %+v, want 3 processed and 3 updated", summary)
	}

	// Two pages, not three: a short page is the end of the walk, so the run
	// does not pay for a query it already knows the answer to.
	wantCursors := []string{"", "doc-b"}
	if len(sel.queries) != len(wantCursors) {
		t.Fatalf("%d selection queries, want %d", len(sel.queries), len(wantCursors))
	}
	for i, want := range wantCursors {
		q := sel.queries[i]
		if q.After != want {
			t.Errorf("query %d cursor = %q, want %q", i, q.After, want)
		}
		if q.WorkspaceID != testWorkspace {
			t.Errorf("query %d workspace = %q, want the fixed workspace", i, q.WorkspaceID)
		}
	}
	// The selection is ordered; the writes inside a page are concurrent, so
	// what is pinned here is the set — every in-workspace message and nothing
	// from the workspace next door.
	written := append([]string{}, writer.order...)
	sort.Strings(written)
	if got := strings.Join(written, ","); got != "doc-a,doc-b,doc-c" {
		t.Errorf("wrote %q, want the three in-workspace documents", got)
	}
}

func TestReprocessLimitBoundsTheRun(t *testing.T) {
	r, _, _, writer := fixture()

	summary, err := r.Run(context.Background(), EmailReprocessOptions{
		WorkspaceID: testWorkspace, Limit: 2, BatchSize: 10,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.Processed != 2 {
		t.Errorf("processed %d, want the 2 the limit allowed", summary.Processed)
	}
	if n := atomic.LoadInt32(&writer.calls); n != 2 {
		t.Errorf("%d writes, want 2", n)
	}
}

func TestReprocessMissingOnlyNarrowsSelection(t *testing.T) {
	r, sel, _, _ := fixture()
	sel.missing["doc-b"] = true

	summary, err := r.Run(context.Background(), EmailReprocessOptions{
		WorkspaceID: testWorkspace, OnlyMissing: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.Processed != 1 || summary.Updated != 1 {
		t.Errorf("summary = %+v, want only the document without metadata", summary)
	}
	if !sel.queries[0].OnlyMissing {
		t.Error("selection did not ask for documents missing metadata")
	}
}

func TestReprocessRequiresAWorkspace(t *testing.T) {
	r, _, _, _ := fixture()
	if _, err := r.Run(context.Background(), EmailReprocessOptions{}); err == nil {
		t.Fatal("Run accepted an empty workspace id")
	}
}

func TestReprocessReportsASelectionFailure(t *testing.T) {
	r, sel, _, _ := fixture()
	sel.err = errors.New("database down")

	summary, err := r.Run(context.Background(), EmailReprocessOptions{WorkspaceID: testWorkspace})
	if err == nil {
		t.Fatal("Run hid a selection failure")
	}
	if summary.Processed != 0 {
		t.Errorf("processed %d, want 0", summary.Processed)
	}
}

// ---- counting ---------------------------------------------------------------

// Re-running is the supported way to converge, so a second pass must rewrite
// the same rows rather than adding a second copy of anything.
func TestReprocessIsIdempotentAcrossRuns(t *testing.T) {
	r, _, _, writer := fixture()
	ctx := context.Background()
	opts := EmailReprocessOptions{WorkspaceID: testWorkspace}

	first, err := r.Run(ctx, opts)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	second, err := r.Run(ctx, opts)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if first.Processed != second.Processed || first.Updated != second.Updated ||
		first.Unreadable != second.Unreadable || first.Failed != second.Failed {
		t.Errorf("second run summary %+v differs from the first %+v", second, first)
	}
	if len(writer.saved) != 3 || len(writer.order) != 3 {
		t.Errorf("%d rows after two runs, want 3", len(writer.order))
	}
}

// A dry run answers "what would this rebuild, and what can it not read?"
// without writing a row.
func TestReprocessDryRunWritesNothing(t *testing.T) {
	r, _, vault, writer := fixture()
	vault.unreadable["raw/b"] = true

	summary, err := r.Run(context.Background(), EmailReprocessOptions{
		WorkspaceID: testWorkspace, DryRun: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !summary.DryRun || summary.Processed != 3 || summary.Updated != 2 || summary.Unreadable != 1 {
		t.Errorf("summary = %+v, want 3 processed, 2 updated, 1 unreadable", summary)
	}
	if n := atomic.LoadInt32(&writer.calls); n != 0 {
		t.Errorf("%d writes during a dry run, want 0", n)
	}
}

func TestReprocessReportsProgressPerBatch(t *testing.T) {
	r, _, _, _ := fixture()
	var seen []int
	if _, err := r.Run(context.Background(), EmailReprocessOptions{
		WorkspaceID: testWorkspace, BatchSize: 1,
		Progress: func(s EmailReprocessSummary) { seen = append(seen, s.Processed) },
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(seen) != 3 || seen[0] != 1 || seen[2] != 3 {
		t.Errorf("progress = %v, want monotonic per-batch totals", seen)
	}
}

// The pool is a bound, not a suggestion: a maintenance walk must not open more
// Tier 1 reads at once than it was told to.
func TestReprocessBoundsConcurrency(t *testing.T) {
	r, sel, vault, _ := fixture()
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("doc-%02d", i)
		key := fmt.Sprintf("raw/%02d", i)
		vault.objects[key] = synthetic(id+"@example.test", "Subject", "sender@example.test")
		sel.docs = append(sel.docs, emailDoc(id, key))
	}
	if _, err := r.Run(context.Background(), EmailReprocessOptions{
		WorkspaceID: testWorkspace, Concurrency: 2, BatchSize: 10,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	vault.mu.Lock()
	peak := vault.peak
	vault.mu.Unlock()
	if peak > 2 {
		t.Errorf("peak concurrent reads = %d, want at most 2", peak)
	}
}

// A cancelled run reports what it finished and does not count the documents it
// never reached as damage.
func TestReprocessStopsOnCancellation(t *testing.T) {
	r, _, _, writer := fixture()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	summary, err := r.Run(ctx, EmailReprocessOptions{WorkspaceID: testWorkspace})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if summary.Processed != 0 || summary.Failed != 0 || summary.Unreadable != 0 {
		t.Errorf("summary = %+v, want nothing counted", summary)
	}
	if n := atomic.LoadInt32(&writer.calls); n != 0 {
		t.Errorf("%d writes after cancellation, want 0", n)
	}
}

// ---- error classification ----------------------------------------------------

// Unreadable and failed are different answers to different questions: one says
// Tier 1 would not give up the bytes, the other says the bytes were read and
// could not be turned into metadata. Neither is silently skipped.
func TestReprocessClassifiesEachOutcome(t *testing.T) {
	r, sel, vault, writer := fixture()

	// Present in Tier 2, unreadable in Tier 1.
	vault.unreadable["raw/b"] = true
	// Read cleanly, refused by the write path.
	writer.failFor["doc-c"] = true
	// A document whose row names no object at all.
	sel.docs = append(sel.docs, domain.Document{DocID: "doc-d", WorkspaceID: testWorkspace})
	// A row pointing outside the workspace's own bucket.
	sel.docs = append(sel.docs, domain.Document{
		DocID: "doc-e", WorkspaceID: testWorkspace, RawURI: "s3://elsewhere/raw/e",
	})
	// Bytes that are not a parsable message.
	vault.objects["raw/f"] = []byte("not a message")
	sel.docs = append(sel.docs, emailDoc("doc-f", "raw/f"))

	summary, err := r.Run(context.Background(), EmailReprocessOptions{WorkspaceID: testWorkspace})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.Processed != 6 {
		t.Fatalf("processed %d, want 6", summary.Processed)
	}
	if summary.Updated != 1 {
		t.Errorf("updated %d, want 1", summary.Updated)
	}
	if summary.Unreadable != 3 {
		t.Errorf("unreadable %d, want 3", summary.Unreadable)
	}
	if summary.Failed != 2 {
		t.Errorf("failed %d, want 2", summary.Failed)
	}
	want := map[string]int{
		ReasonReprocessUnreadable:   1,
		ReasonReprocessNoObject:     1,
		ReasonReprocessBadObjectURI: 1,
		ReasonReprocessWriteFailed:  1,
		ReasonReprocessParseFailed:  1,
	}
	for code, n := range want {
		if summary.Reasons[code] != n {
			t.Errorf("reason %s = %d, want %d (all: %v)", code, summary.Reasons[code], n, summary.Reasons)
		}
	}
}

func TestClassifyEmailReprocess(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		wantOutcome EmailReprocessOutcome
		wantCode    string
	}{
		{"success", nil, EmailReprocessUpdated, ""},
		{"unreadable", &reprocessError{EmailReprocessUnreadable, ReasonReprocessUnreadable,
			errors.New("read failed")}, EmailReprocessUnreadable, ReasonReprocessUnreadable},
		{"wrapped", fmt.Errorf("context: %w", &reprocessError{EmailReprocessFailed,
			ReasonReprocessParseFailed, errors.New("bad")}), EmailReprocessFailed, ReasonReprocessParseFailed},
		{"cancelled", context.Canceled, EmailReprocessCancelled, ""},
		{"deadline", context.DeadlineExceeded, EmailReprocessCancelled, ""},
		{"unlabelled", errors.New("something else"), EmailReprocessFailed, ReasonReprocessUnclassified},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			outcome, code := classifyEmailReprocess(c.err)
			if outcome != c.wantOutcome || code != c.wantCode {
				t.Errorf("classify(%v) = (%s, %s), want (%s, %s)",
					c.err, outcome, code, c.wantOutcome, c.wantCode)
			}
		})
	}
}

// An undecodable body is its own class: the message was read, and guessing at
// its charset would be silent content corruption rather than a loud failure.
func TestReprocessClassifiesAnUnknownCharset(t *testing.T) {
	r, sel, vault, _ := fixture()
	vault.objects["raw/g"] = []byte("Subject: Synthetic\r\n" +
		"Content-Type: text/plain; charset=definitely-not-a-charset\r\n\r\nbody\r\n")
	sel.docs = append(sel.docs, emailDoc("doc-g", "raw/g"))

	summary, err := r.Run(context.Background(), EmailReprocessOptions{WorkspaceID: testWorkspace})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.Reasons[ReasonReprocessUnknownEncode] != 1 {
		t.Errorf("reasons = %v, want one %s", summary.Reasons, ReasonReprocessUnknownEncode)
	}
}

// The reprocessor is the same mapping the live worker uses, so what it writes
// must be what the parser said — not a value re-derived from Tier 2.
func TestReprocessWritesTheParsedMetadata(t *testing.T) {
	r, _, _, writer := fixture()
	if _, err := r.Run(context.Background(), EmailReprocessOptions{WorkspaceID: testWorkspace}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, ok := writer.saved["doc-a"]
	if !ok {
		t.Fatal("doc-a was not written")
	}
	parsed, err := email.ParseEmail(synthetic("msg-a@example.test", "Subject a", "sender@example.test"))
	if err != nil {
		t.Fatal(err)
	}
	want := emailMessageOf("doc-a", testWorkspace, parsed)
	if got.MessageID != want.MessageID || got.SubjectRaw != want.SubjectRaw ||
		len(got.Addresses) != len(want.Addresses) || got.WorkspaceID != testWorkspace {
		t.Errorf("wrote %+v, want the parser's own mapping %+v", got, want)
	}
}
