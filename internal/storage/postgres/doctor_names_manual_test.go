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
