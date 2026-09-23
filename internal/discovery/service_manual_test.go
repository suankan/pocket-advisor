//go:build manual

package discovery

import (
	"context"
	"errors"
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
	scan, err := s.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Docs.ValidateRetentionScan(ctx, scan); err != nil {
		t.Fatal(err)
	}
	ready, _, _, _, err := s.Jobs.Counts(ctx)
	if err != nil || ready != 10002 {
		t.Fatalf("admission did not enqueue beyond the existing backlog: %d %v", ready, err)
	}
}

func TestPartialScanCannotAuthorizePruning(t *testing.T) {
	s, root := scanFixture(t)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("Observed before traversal fails."), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(root, "z.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(257 << 20); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	scan, err := s.Scan(context.Background())
	if err == nil {
		t.Fatal("oversized original did not fail the scan")
	}
	if err := s.Docs.ValidateRetentionScan(context.Background(), scan); !errors.Is(err, postgres.ErrInvalidRetentionScan) {
		t.Fatalf("partial scan authorized pruning: %v", err)
	}
	var count int
	if err := s.DB.Pool.QueryRow(context.Background(), `SELECT count(*) FROM source_occurrences`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("test did not exercise partial admission: %d %v", count, err)
	}
}

func TestBackfillEstablishesFailedContainerWithoutChangingStatus(t *testing.T) {
	s, root := scanFixture(t)
	ctx := context.Background()
	data := []byte("From: sender@example.test\r\nSubject: Synthetic\r\nMIME-Version: 1.0\r\nContent-Type: text/plain\r\n\r\nBody.\r\n")
	id, _, err := s.Docs.CreateStub(ctx, &domain.Document{DocID: domain.NewDocID(), RawSHA256: domain.SHA256Hex(data), DocType: "email"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Docs.UpdateStatus(ctx, id, domain.StatusFailed, domain.ReasonEmailParseFailed); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "message.eml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	scan, err := s.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.Docs.Status(ctx, id)
	if err != nil || state != domain.StatusFailed {
		t.Fatalf("backfill changed status: %s %v", state, err)
	}
	retained, err := s.Docs.RetentionForScan(ctx, scan)
	if err != nil || retained.UnestablishedContainers != 0 {
		t.Fatalf("failed status blocked established containment: %+v %v", retained, err)
	}
}
