//go:build manual

package postgres

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/source"
)

// Doctor names the documents behind a reason the way the run summary does,
// newest first and capped unless every one is asked for, and counts those with
// no job, which were recorded before the job queue.
func TestDocumentsByReasonNamesAndCountsJobless(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	inventory := NewSourceRepo(db)
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
	age := func(id, ago string) {
		t.Helper()
		if _, err := db.Pool.Exec(ctx, "UPDATE documents SET updated_at = now() - $2::interval WHERE doc_id = $1", id, ago); err != nil {
			t.Fatal(err)
		}
	}
	legacy := func(path, ago string) {
		t.Helper()
		id, _, err := docs.CreateStub(ctx, &domain.Document{DocID: domain.NewDocID(), RawSHA256: domain.SHA256Hex([]byte(path)), DocType: "email"})
		if err != nil {
			t.Fatal(err)
		}
		observe(id, path)
		if err := docs.UpdateStatus(ctx, id, domain.StatusFailed, domain.ReasonUnclassified); err != nil {
			t.Fatal(err)
		}
		age(id, ago)
	}
	legacy("old.eml", "2 days")
	legacy("older.eml", "3 days")
	current, _ := addJob(t, db, docs, jobs, SubjectPDFs)
	observe(current, "now.pdf")
	if err := jobs.Fail(ctx, claimOne(t, jobs, SubjectPDFs), domain.ReasonUnclassified); err != nil {
		t.Fatal(err)
	}
	encrypted, _, err := docs.CreateStub(ctx, &domain.Document{DocID: domain.NewDocID(), RawSHA256: domain.SHA256Hex([]byte("locked")), DocType: "pdf"})
	if err != nil {
		t.Fatal(err)
	}
	observe(encrypted, "locked.pdf")
	if err := docs.UpdateStatus(ctx, encrypted, domain.StatusSkipped, domain.ReasonPDFEncrypted); err != nil {
		t.Fatal(err)
	}
	unsupported, _, err := docs.CreateStub(ctx, &domain.Document{DocID: domain.NewDocID(), RawSHA256: domain.SHA256Hex([]byte("odd")), DocType: "legacy-office"})
	if err != nil {
		t.Fatal(err)
	}
	if err := docs.UpdateStatus(ctx, unsupported, domain.StatusSkipped, domain.ReasonUnsupportedFormat); err != nil {
		t.Fatal(err)
	}

	q := NewDoctorQueries(db)
	all, err := q.DocumentsByReason(ctx, domain.StatusFailed, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := all[string(domain.ReasonUnclassified)]
	if got.Jobless != 2 || !slices.Equal(got.Locations, []string{"root/now.pdf", "root/old.eml", "root/older.eml"}) {
		t.Fatalf("all failures = %+v, want 2 jobless and every location newest first", got)
	}
	capped, err := q.DocumentsByReason(ctx, domain.StatusFailed, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := capped[string(domain.ReasonUnclassified)]; got.Jobless != 2 || !slices.Equal(got.Locations, []string{"root/now.pdf"}) {
		t.Fatalf("capped failures = %+v, want the jobless count of all and one location", got)
	}

	locked, err := q.DocumentsByReason(ctx, domain.StatusSkipped, []string{string(domain.ReasonPDFEncrypted)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(locked) != 1 || !slices.Equal(locked[string(domain.ReasonPDFEncrypted)].Locations, []string{"root/locked.pdf"}) {
		t.Fatalf("password-protected = %+v, want only the encrypted PDF", locked)
	}
}

// Naming reasons selects every FAILED document carrying one, whatever its
// class, oldest first, and nothing else; naming none selects every failure.
func TestFAILEDWithReasonsSelectsOnlyThoseReasons(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	var want []string
	for _, reason := range []domain.FailureReason{domain.ReasonUnclassified, domain.ReasonUnknownEncoding, domain.ReasonUnclassified} {
		id, _ := addJob(t, db, docs, jobs, SubjectPDFs)
		if err := jobs.Fail(ctx, claimOne(t, jobs, SubjectPDFs), reason); err != nil {
			t.Fatal(err)
		}
		if reason == domain.ReasonUnclassified {
			want = append(want, id)
		}
	}
	got, err := NewDoctorQueries(db).FAILEDWithReasons(ctx, []string{string(domain.ReasonUnclassified)}, 10)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, d := range got {
		ids = append(ids, d.DocID)
		if d.Jobless {
			t.Errorf("%s reported jobless", d.DocID)
		}
	}
	if !slices.Equal(ids, want) {
		t.Fatalf("selected %v, want %v", ids, want)
	}
	both, err := NewDoctorQueries(db).FAILEDWithReasons(ctx, []string{string(domain.ReasonUnclassified), string(domain.ReasonUnknownEncoding)}, 10)
	if err != nil || len(both) != 3 {
		t.Fatalf("two reasons: %d selected, %v", len(both), err)
	}
	all, err := NewDoctorQueries(db).FAILEDWithReasons(ctx, nil, 10)
	if err != nil || len(all) != 3 {
		t.Fatalf("every failure: %d selected, %v", len(all), err)
	}
}

// A document is described by where its file is, and an email, or a member of
// one, by the message's sender, subject and date.
func TestDescribeNamesFilesAndMessages(t *testing.T) {
	db, docs, _ := queueSetup(t)
	ctx := context.Background()
	inventory := NewSourceRepo(db)
	scan, err := inventory.BeginScan(ctx, []string{"root"})
	if err != nil {
		t.Fatal(err)
	}
	stub := func(name, docType string) string {
		t.Helper()
		id, _, err := docs.CreateStub(ctx, &domain.Document{DocID: domain.NewDocID(), RawSHA256: domain.SHA256Hex([]byte(name)), SourceName: name, DocType: docType})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	message := stub("1.eml", "email")
	if err := inventory.Observe(ctx, scan, message, source.File{RootKey: "root", RelativePath: "mail/1.eml", DisplayPath: "root/mail/1.eml", ModifiedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	sent := time.Date(2018, 11, 5, 10, 0, 0, 0, time.UTC)
	if err := docs.SaveEmailText(ctx, message, "body", domain.EmailHeaders{From: "a@example.com", Subject: "Policy", Date: sent}); err != nil {
		t.Fatal(err)
	}
	attachment := stub("policy.pdf", "pdf")
	if err := docs.RecordPlacement(ctx, message, attachment); err != nil {
		t.Fatal(err)
	}
	report := stub("report.pdf", "pdf")
	if err := inventory.Observe(ctx, scan, report, source.File{RootKey: "root", RelativePath: "docs/report.pdf", DisplayPath: "root/docs/report.pdf", ModifiedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	got, err := NewDoctorQueries(db).Describe(ctx, []string{message, attachment, report, domain.NewDocID()})
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]Description{
		message:    {Location: "root/mail/1.eml", From: "a@example.com", Subject: "Policy", Date: sent},
		attachment: {Location: "policy.pdf in root/mail/1.eml", From: "a@example.com", Subject: "Policy", Date: sent, InEmail: true},
		report:     {Location: "root/docs/report.pdf"},
	} {
		d := got[id]
		if d.Location != want.Location || d.From != want.From || d.Subject != want.Subject || !d.Date.Equal(want.Date) || d.InEmail != want.InEmail {
			t.Errorf("%s described %+v, want %+v", id, d, want)
		}
	}
	if len(got) != 3 {
		t.Errorf("described %d documents, want the 3 that exist", len(got))
	}
}
