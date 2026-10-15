//go:build manual

package worker

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	ingestionv1 "github.com/suankan/pocket-advisor/api/proto/v1/gen"
	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/engine/email"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/trace"
)

// Database-backed checks of how the email worker admits members. Manual
// because what is under test is PostgreSQL's locking.
//
//	Run: EMAIL_DSN=postgres://<role>@localhost:5432/<disposable-db> \
//		go test -tags manual ./internal/worker/ -run Attachment -v

func admissionFixture(t *testing.T) (*postgres.DB, *EmailWorker, string) {
	t.Helper()
	dsn := os.Getenv("EMAIL_DSN")
	if dsn == "" {
		t.Skip("requires disposable EMAIL_DSN")
	}
	ctx := context.Background()
	admin, err := postgres.Connect(ctx, dsn, 2)
	if err != nil {
		t.Fatal(err)
	}
	schema := "admit_" + time.Now().Format("150405000000000")
	if _, err := admin.Pool.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema+",public")
	u.RawQuery = q.Encode()
	scoped := u.String()
	db, err := postgres.Connect(ctx, scoped, 6)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(); _, _ = admin.Pool.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`); admin.Close() })
	if err := db.ApplySchema(ctx, postgres.SchemaMetadata{WorkspaceID: "synthetic", EmbedModel: "synthetic", EmbedDim: 8}); err != nil {
		t.Fatal(err)
	}
	return db, workerOn(db), scoped
}

// bytesSource serves fixed bytes for any reference.
type bytesSource []byte

func (b bytesSource) Read(context.Context, string, string) ([]byte, error) { return b, nil }

// An archive the unroller cannot read reads the same way on every attempt, so
// it is declined rather than failed, under the reason that names why.
func TestUnreadableArchiveIsDeclined(t *testing.T) {
	_, ew, _ := admissionFixture(t)
	ctx := context.Background()
	data := []byte("7z\xbc\xaf\x27\x1c synthetic, not unrolled")
	sum := domain.SHA256Hex(data)
	id, _, err := ew.Docs.CreateStub(ctx, &domain.Document{DocID: domain.NewDocID(), RawSHA256: sum, DocType: "archive", MimeType: "application/x-7z-compressed"})
	if err != nil {
		t.Fatal(err)
	}
	ew.Sources = bytesSource(data)
	ew.Log = slog.New(slog.DiscardHandler)
	payload, err := proto.Marshal(&ingestionv1.ProcessEmailCommand{Metadata: &ingestionv1.DocumentMetadata{
		DocId: id, RawSha256: sum, MimeType: "application/x-7z-compressed", SourceFilename: "letters.7z", Traceparent: trace.NewTraceparent()}})
	if err != nil {
		t.Fatal(err)
	}
	err = ew.Handle(ctx, postgres.Job{DocID: id, Payload: payload})
	var declined *Declined
	if !errors.As(err, &declined) || declined.Reason != domain.ReasonUnsupportedFormat {
		t.Fatalf("err = %v, want a %s decline", err, domain.ReasonUnsupportedFormat)
	}
}

func workerOn(db *postgres.DB) *EmailWorker {
	return &EmailWorker{Inventory: postgres.NewSourceRepo(db), Docs: postgres.NewDocumentRepo(db), Bus: postgres.NewJobs(db)}
}

// connectAs opens a pool on the fixture's schema whose sessions carry name as
// their application_name, so a test can see from pg_stat_activity what that
// pool's transaction is waiting for.
func connectAs(t *testing.T, scoped, name string) *postgres.DB {
	t.Helper()
	u, err := url.Parse(scoped)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("application_name", name)
	u.RawQuery = q.Encode()
	db, err := postgres.Connect(context.Background(), u.String(), 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

func stageEmail(t *testing.T, w *EmailWorker, body string) *ingestionv1.DocumentMetadata {
	t.Helper()
	sum := domain.SHA256Hex([]byte(body))
	id, _, err := w.Docs.CreateStub(context.Background(), &domain.Document{DocID: domain.NewDocID(), RawSHA256: sum, DocType: "email", MimeType: "message/rfc822"})
	if err != nil {
		t.Fatal(err)
	}
	return &ingestionv1.DocumentMetadata{DocId: id, RawSha256: sum, MimeType: "message/rfc822", Traceparent: trace.NewTraceparent()}
}

func count(t *testing.T, db *postgres.DB, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The second email to carry an attachment whose extraction is running links
// it and moves on. Its stage handler holds the row for the whole extraction,
// and before admission resolved known content by a plain read this email
// waited for all of it.
func TestKnownAttachmentIsLinkedWithoutWaitingOnItsExtraction(t *testing.T) {
	db, w, _ := admissionFixture(t)
	ctx := context.Background()
	pdf := []byte("%PDF-1.4 synthetic attachment under extraction")
	held, _, err := w.Docs.CreateStub(ctx, &domain.Document{DocID: domain.NewDocID(), RawSHA256: domain.SHA256Hex(pdf), DocType: "pdf"})
	if err != nil {
		t.Fatal(err)
	}
	parent := stageEmail(t, w, "second email carrying the attachment")

	handler, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Rollback(ctx)
	if _, err := handler.Exec(ctx, `UPDATE documents SET processing_status='PROCESSING' WHERE doc_id=$1`, held); err != nil {
		t.Fatal(err)
	}

	deadline, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	err = db.Transaction(deadline, func(ctx context.Context) error {
		for _, a := range admissionOrder([]email.Child{{Filename: "terms.pdf", Selector: "mime:1", Data: pdf}}) {
			if err := w.dispatchChild(ctx, parent, a); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("admission waited on the attachment's stage handler: %v", err)
	}
	if n := count(t, db, `SELECT count(*) FROM source_members WHERE parent_id=$1 AND child_id=$2`, parent.DocId, held); n != 1 {
		t.Errorf("member routes = %d, want 1", n)
	}
	if n := count(t, db, `SELECT count(*) FROM document_placements WHERE container_doc_id=$1 AND contained_doc_id=$2`, parent.DocId, held); n != 1 {
		t.Errorf("placements = %d, want 1", n)
	}
	if n := count(t, db, `SELECT count(*) FROM ingestion_jobs WHERE doc_id=$1`, held); n != 0 {
		t.Errorf("known content was queued again: %d jobs", n)
	}
}

// A member whose name was recovered from undeclared legacy bytes keeps those
// bytes and how they were read beside its stored name, and a text attachment
// containing NUL is declined rather than failing the email on save.
func TestRecoveredNameKeepsItsBytesAndNULTextIsSkipped(t *testing.T) {
	db, w, _ := admissionFixture(t)
	ctx := context.Background()
	parent := stageEmail(t, w, "email carrying legacy-named members")
	detected := []byte("%PDF-1.4 synthetic member with a detected name")
	replaced := []byte("%PDF-1.4 synthetic member with a replaced name")
	// Past the sniffer's header window, so the member still routes as text.
	nul := []byte(strings.Repeat("key=value\n", 500) + "\x00")
	children := []email.Child{
		{Filename: "Отчёт.pdf", Selector: "mime-v1:0", Data: detected, RawName: []byte("\x8e\xe2\xe7\xf1\xe2.pdf"), NameEncoding: "ibm866"},
		{Filename: "�.pdf", Selector: "mime-v1:1", Data: replaced, RawName: []byte("\xb0\xa1.pdf")},
		{Filename: "settings.conf", Selector: "mime-v1:2", Data: nul},
	}
	err := db.Transaction(ctx, func(ctx context.Context) error {
		for _, a := range admissionOrder(children) {
			if err := w.dispatchChild(ctx, parent, a); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("admission failed: %v", err)
	}
	meta := func(data []byte, key string) string {
		var v string
		if err := db.Pool.QueryRow(ctx, `SELECT COALESCE(metadata_headers->>$2, '') FROM documents WHERE raw_sha256=$1`,
			domain.SHA256Hex(data), key).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if got := meta(detected, "source_name_raw"); got != "8ee2e7f1e22e706466" {
		t.Errorf("detected member raw name = %q", got)
	}
	if got := meta(detected, "source_name_encoding"); got != "ibm866" {
		t.Errorf("detected member encoding = %q", got)
	}
	if got := meta(replaced, "source_name_encoding"); got != "unknown" {
		t.Errorf("replaced member encoding = %q, want unknown", got)
	}
	if got := meta(nul, "source_name_raw"); got != "" {
		t.Errorf("a UTF-8 name was marked as recovered: %q", got)
	}
	if got := meta(nul, "reason"); got != string(domain.ReasonNULBytes) {
		t.Errorf("NUL text member reason = %q, want %s", got, domain.ReasonNULBytes)
	}
}

// Two emails carry the same two new attachments in opposite order. The first
// admits one member and holds its transaction open until the second has either
// admitted a member of its own or is blocked behind the first; only then does
// the first admit its other member. Admitting in message order deadlocks on
// that schedule every time: each holds one uncommitted stub the other needs.
// Admission order makes the second wait on the first's first member instead,
// and both commit with one document per attachment and every containment
// recorded.
func TestSharedAttachmentsInOppositeOrderDoNotDeadlock(t *testing.T) {
	db, w, scoped := admissionFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p := email.Child{Filename: "p.pdf", Selector: "mime:1", Data: []byte("%PDF-1.4 synthetic shared attachment P")}
	q := email.Child{Filename: "q.pdf", Selector: "mime:2", Data: []byte("%PDF-1.4 synthetic shared attachment Q")}
	first, second := stageEmail(t, w, "first email"), stageEmail(t, w, "second email")
	dbA, dbB := connectAs(t, scoped, "admit_first"), connectAs(t, scoped, "admit_second")
	wA, wB := workerOn(dbA), workerOn(dbB)

	// The first email resumes once the second has admitted a member or is
	// blocked by the first, whichever happens.
	aAdmitted, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	proceed := func() { once.Do(func() { close(resume) }) }
	go func() {
		for {
			var blocked bool
			err := db.Pool.QueryRow(ctx, `SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity b, pg_stat_activity a
				WHERE b.application_name = 'admit_second' AND a.application_name = 'admit_first'
				  AND a.pid = ANY(pg_blocking_pids(b.pid)))`).Scan(&blocked)
			if err != nil || blocked {
				proceed()
				return
			}
			select {
			case <-resume:
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()

	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs[0] = dbA.Transaction(ctx, func(ctx context.Context) error {
			for i, a := range admissionOrder([]email.Child{p, q}) {
				if i == 1 {
					close(aAdmitted)
					<-resume
				}
				if err := wA.dispatchChild(ctx, first, a); err != nil {
					return err
				}
			}
			return nil
		})
	}()
	go func() {
		defer wg.Done()
		<-aAdmitted
		errs[1] = dbB.Transaction(ctx, func(ctx context.Context) error {
			for i, a := range admissionOrder([]email.Child{q, p}) {
				if err := wB.dispatchChild(ctx, second, a); err != nil {
					return err
				}
				if i == 0 {
					proceed()
				}
			}
			return nil
		})
	}()
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("email %d: %v", i+1, err)
		}
	}

	for _, c := range []email.Child{p, q} {
		if n := count(t, db, `SELECT count(*) FROM documents WHERE raw_sha256=$1`, domain.SHA256Hex(c.Data)); n != 1 {
			t.Errorf("%s: %d documents, want 1", c.Filename, n)
		}
	}
	if n := count(t, db, `SELECT count(*) FROM document_placements WHERE container_doc_id = ANY($1::uuid[])`, []string{first.DocId, second.DocId}); n != 4 {
		t.Errorf("placements = %d, want both attachments in both emails", n)
	}
	if n := count(t, db, `SELECT count(*) FROM ingestion_jobs WHERE subject=$1`, postgres.SubjectPDFs); n != 2 {
		t.Errorf("extraction jobs = %d, want one per attachment", n)
	}
}
