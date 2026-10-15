//go:build manual

package postgres

import (
	"context"
	"slices"
	"testing"
	"time"

	ingestionv1 "github.com/suankan/pocket-advisor/api/proto/v1/gen"
	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/source"
)

// A run summary is scoped by the run's start on the database clock: failures
// and documents from before it are earlier, everything the run created or
// finished a job for is this run's, including documents discovery settled
// without any job. Failures and this run's password skips are named, members
// by the container they came from, and a document whose own file is gone by a
// container that still holds it.
func TestRunSummaryScopesTheRunByItsStart(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	inventory := NewSourceRepo(db)

	earlier, _ := addJob(t, db, docs, jobs, SubjectPDFs)
	if _, err := db.Pool.Exec(ctx, `UPDATE documents SET source_filename = 'saved.zip' WHERE doc_id = $1`, earlier); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Fail(ctx, claimOne(t, jobs, SubjectPDFs), domain.ReasonUnknownEncoding); err != nil {
		t.Fatal(err)
	}
	since, err := jobs.Clock(ctx)
	if err != nil {
		t.Fatal(err)
	}

	scan, err := inventory.BeginScan(ctx, []string{"root"})
	if err != nil {
		t.Fatal(err)
	}
	observe := func(docID, path string) {
		t.Helper()
		if err := inventory.Observe(ctx, scan, docID, source.File{RootKey: "root", RelativePath: path, DisplayPath: "root/" + path, ModifiedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}

	failed, _ := addJob(t, db, docs, jobs, SubjectPDFs)
	observe(failed, "a.pdf")
	if err := jobs.Fail(ctx, claimOne(t, jobs, SubjectPDFs), domain.ReasonOCRFailed); err != nil {
		t.Fatal(err)
	}

	mail, _, err := docs.CreateStub(ctx, &domain.Document{DocID: domain.NewDocID(), RawSHA256: domain.SHA256Hex([]byte("mail")), DocType: "email"})
	if err != nil {
		t.Fatal(err)
	}
	observe(mail, "mail.eml")
	if err := docs.UpdateStatus(ctx, mail, domain.StatusCompleted, ""); err != nil {
		t.Fatal(err)
	}
	locked, _, err := docs.CreateStub(ctx, &domain.Document{DocID: domain.NewDocID(), ParentDocID: mail, RawSHA256: domain.SHA256Hex([]byte("locked")), SourceName: "letter.pdf", DocType: "pdf"})
	if err != nil {
		t.Fatal(err)
	}
	meta := &ingestionv1.DocumentMetadata{DocId: locked, Traceparent: "synthetic"}
	if err := jobs.Publish(ctx, SubjectPDFs, &ingestionv1.ProcessPdfCommand{Metadata: meta, SourceRef: source.Ref(locked)}, ""); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Run(ctx, claimOne(t, jobs, SubjectPDFs), func(ctx context.Context) error {
		return docs.UpdateStatus(ctx, locked, domain.StatusSkipped, domain.ReasonPDFEncrypted)
	}); err != nil {
		t.Fatal(err)
	}

	// The earlier failure was first seen as a file of its own, since deleted;
	// the same content is still attached to the message.
	observe(earlier, "saved.zip")
	if _, err := db.Pool.Exec(ctx, `UPDATE source_occurrences SET available = false WHERE doc_id = $1`, earlier); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO document_placements (container_doc_id, contained_doc_id) VALUES ($1, $2)`, mail, earlier); err != nil {
		t.Fatal(err)
	}

	declined, _, err := docs.CreateStub(ctx, &domain.Document{DocID: domain.NewDocID(), RawSHA256: domain.SHA256Hex([]byte("declined")), DocType: "legacy-office"})
	if err != nil {
		t.Fatal(err)
	}
	if err := docs.UpdateStatus(ctx, declined, domain.StatusSkipped, domain.ReasonUnsupportedFormat); err != nil {
		t.Fatal(err)
	}
	addJob(t, db, docs, jobs, SubjectPDFs) // queued, not yet run

	s, err := jobs.RunSummary(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	if s.Files != 3 || s.NewOriginals != 4 || s.Unfinished != 1 || s.Waiting != 0 {
		t.Fatalf("files %d, new %d, unfinished %d, waiting %d", s.Files, s.NewOriginals, s.Unfinished, s.Waiting)
	}
	for _, want := range []struct {
		status domain.Status
		tally  Tally
	}{
		{domain.StatusFailed, Tally{"OCR_FAILED", 1}},
		{domain.StatusSkipped, Tally{"PDF_ENCRYPTED", 1}},
		{domain.StatusSkipped, Tally{"UNSUPPORTED_FORMAT", 1}},
		{domain.StatusCompleted, Tally{"", 1}},
		{domain.StatusPending, Tally{"", 1}},
	} {
		if !slices.Contains(s.Settled[want.status], want.tally) {
			t.Errorf("settled %s = %+v, want %+v among them", want.status, s.Settled[want.status], want.tally)
		}
	}
	if !slices.Equal(s.FailedThisRun, []Tally{{"OCR_FAILED", 1}}) || !slices.Equal(s.FailedEarlier, []Tally{{"UNKNOWN_ENCODING", 1}}) {
		t.Fatalf("failed this run %+v, earlier %+v", s.FailedThisRun, s.FailedEarlier)
	}
	if s.Locked != 1 || s.LockedThisRun != 1 {
		t.Fatalf("locked %d, this run %d", s.Locked, s.LockedThisRun)
	}
	want := []AttentionDoc{
		{domain.StatusFailed, "OCR_FAILED", true, "root/a.pdf"},
		{domain.StatusSkipped, "PDF_ENCRYPTED", true, "letter.pdf in root/mail.eml"},
		{domain.StatusFailed, "UNKNOWN_ENCODING", false, "saved.zip in root/mail.eml"},
	}
	if !slices.Equal(s.Attention, want) || s.AttentionTotal != 3 {
		t.Fatalf("attention = %+v (total %d), want %+v", s.Attention, s.AttentionTotal, want)
	}
}
