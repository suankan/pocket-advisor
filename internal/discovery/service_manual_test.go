//go:build manual

package discovery

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/source"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
)

func scanFixture(t *testing.T) (*Service, string) {
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
	schema := "scan_" + time.Now().Format("150405000000000")
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
	db, err := postgres.Connect(ctx, u.String(), 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(); _, _ = admin.Pool.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`); admin.Close() })
	if err := db.ApplySchema(ctx, postgres.SchemaMetadata{WorkspaceID: "synthetic", EmbedModel: "synthetic", EmbedDim: 8}); err != nil {
		t.Fatal(err)
	}
	unlock, err := db.LockMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
	root := t.TempDir()
	inv := postgres.NewSourceRepo(db)
	r, err := source.New([]string{root}, nil, inv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return &Service{DB: db, Sources: r, Inventory: inv, Docs: postgres.NewDocumentRepo(db), Jobs: postgres.NewJobs(db)}, root
}

func TestScanAdmitsWithLargeUndrainedBacklog(t *testing.T) {
	s, root := scanFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, _, err := s.Docs.CreateStub(ctx, &domain.Document{DocID: domain.NewDocID(), RawSHA256: domain.SHA256Hex([]byte("old")), DocType: "text"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Pool.Exec(ctx, `INSERT INTO ingestion_jobs(doc_id,subject,revision,payload,traceparent)
	 SELECT $1,'synthetic',n::text,''::bytea,'' FROM generate_series(1,10001) n`, id); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("A new synthetic document."), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	ready, _, _, _, err := s.Jobs.Counts(ctx)
	if err != nil || ready != 10002 {
		t.Fatalf("admission did not enqueue beyond the existing backlog: %d %v", ready, err)
	}
}

// Ingestion never deletes, so a jobless PENDING row whose original is gone
// survives until --reconcile-deletions. Resuming it would fail to open the
// source and end every run; it must be skipped, not retried.
func TestResumeLegacySkipsRowsWhoseOriginalsAreGone(t *testing.T) {
	s, root := scanFixture(t)
	ctx := context.Background()
	orphan, _, err := s.Docs.CreateStub(ctx, &domain.Document{DocID: domain.NewDocID(),
		RawSHA256: domain.SHA256Hex([]byte("removed synthetic original")), DocType: "pdf", MimeType: "application/pdf"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "present.txt"), []byte("A present synthetic document."), 0600); err != nil {
		t.Fatal(err)
	}
	scan, err := s.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResumeLegacy(ctx, scan); err != nil {
		t.Fatalf("unreachable legacy row failed the run: %v", err)
	}
	var jobs int
	if err := s.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM ingestion_jobs WHERE doc_id=$1`, orphan).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("unreachable legacy row was resumed: %d %v", jobs, err)
	}
}

// Admission of content that already exists must not queue behind a worker's
// open transaction on that document. CreateStub's upsert takes the row lock
// even when the row exists, so a duplicate copy met mid-scan stalled the whole
// serial walk for as long as the worker's handler ran.
func TestAdmitDoesNotWaitOnDocumentRowLockedByWorker(t *testing.T) {
	s, root := scanFixture(t)
	ctx := context.Background()
	content := []byte("A synthetic document a worker is busy with.")
	id, _, err := s.Docs.CreateStub(ctx, &domain.Document{DocID: domain.NewDocID(), RawSHA256: domain.SHA256Hex(content), DocType: "text", MimeType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Docs.UpdateStatus(ctx, id, domain.StatusCompleted, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "copy.txt"), content, 0600); err != nil {
		t.Fatal(err)
	}
	// A worker's stage transaction: it has written the document row and has
	// not committed.
	worker, err := s.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Rollback(ctx)
	if _, err := worker.Exec(ctx, `UPDATE documents SET updated_at=now() WHERE doc_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	scan, err := s.Inventory.BeginScan(ctx, s.Sources.RootKeys())
	if err != nil {
		t.Fatal(err)
	}
	walkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	err = s.Sources.Walk(walkCtx, func(f source.File, data []byte) error { return s.admit(walkCtx, scan, f, data) })
	if err != nil {
		t.Fatalf("admission waited on the worker's row lock: %v", err)
	}
	var seen int
	if err := s.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM source_occurrences WHERE doc_id=$1 AND scan_id=$2`, id, scan).Scan(&seen); err != nil || seen != 1 {
		t.Fatalf("duplicate was not observed: %d %v", seen, err)
	}
}
