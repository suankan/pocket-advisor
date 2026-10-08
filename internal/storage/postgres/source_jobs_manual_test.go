//go:build manual

package postgres

import (
	"context"
	ingestionv1 "github.com/suankan/pocket-advisor/api/proto/v1/gen"
	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/source"
	"testing"
	"time"
)

func setupSourceJob(t *testing.T) (*DB, *DocumentRepo, *Jobs, string, *ingestionv1.ProcessPdfCommand) {
	t.Helper()
	db, _ := scratch(t, "source_"+t.Name())
	ctx := context.Background()
	if err := db.ApplySchema(ctx, stageBMeta()); err != nil {
		t.Fatal(err)
	}
	docs := NewDocumentRepo(db)
	id := domain.NewDocID()
	hash := domain.SHA256Hex([]byte("synthetic"))
	if _, _, err := docs.CreateStub(ctx, &domain.Document{DocID: id, RawSHA256: hash, DocType: "pdf"}); err != nil {
		t.Fatal(err)
	}
	cmd := &ingestionv1.ProcessPdfCommand{Metadata: &ingestionv1.DocumentMetadata{DocId: id, RawSha256: hash, Traceparent: "synthetic"}, SourceRef: source.Ref(id)}
	return db, docs, NewJobs(db), id, cmd
}
func TestSourceInventoryLocationsAndScanCompleteness(t *testing.T) {
	db, docs, _, id, _ := setupSourceJob(t)
	ctx := context.Background()
	inventory := NewSourceRepo(db)
	scan, err := inventory.BeginScan(ctx, []string{"opaque-binding"})
	if err != nil {
		t.Fatal(err)
	}
	if err := inventory.Observe(ctx, scan, id, source.File{RootKey: "opaque-binding", RelativePath: "file.pdf", DisplayPath: "file.pdf", ModifiedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	child := stageDoc(t, docs, "child", "text", "")
	if err := inventory.RecordMember(ctx, id, child, "zip-v1:1"); err != nil {
		t.Fatal(err)
	}
	if err := inventory.FinishScan(ctx, scan); err != nil {
		t.Fatal(err)
	}
	locations, err := inventory.Locations(ctx, []string{child})
	if err != nil || len(locations[child]) != 1 || locations[child][0].Selectors[0] != "zip-v1:1" {
		t.Fatalf("locations=%v %v", locations, err)
	}
	// Re-applying the source schema is a supported no-op, so a rerun of the
	// same binary must not disturb an identity or an observation.
	if err := db.ApplySourceSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, err := docs.Exists(ctx, id); err != nil || !ok {
		t.Fatalf("identity lost %v", err)
	}
	incomplete, err := inventory.BeginScan(ctx, []string{"opaque-binding"})
	if err != nil {
		t.Fatal(err)
	}
	locations, _ = inventory.Locations(ctx, []string{id})
	if locations[id][0].Status != "last_observed" {
		t.Fatal("partial scan proved absence")
	}
	if err := inventory.FinishScan(ctx, incomplete); err != nil {
		t.Fatal(err)
	}
	locations, _ = inventory.Locations(ctx, []string{id})
	if locations[id][0].Status != "missing" {
		t.Fatal("complete scan missed absence")
	}
}
func TestSourceWipeRemovesInventoryAndJobs(t *testing.T) {
	db, docs, jobs, id, cmd := setupSourceJob(t)
	ctx := context.Background()
	inv := NewSourceRepo(db)
	scan, err := inv.BeginScan(ctx, []string{"synthetic-root"})
	if err != nil {
		t.Fatal(err)
	}
	if err := inv.Observe(ctx, scan, id, source.File{RootKey: "synthetic-root", RelativePath: "file.pdf", DisplayPath: "file.pdf", ModifiedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := inv.FinishScan(ctx, scan); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Publish(ctx, SubjectPDFs, cmd, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := docs.DeleteWorkspace(ctx); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"documents", "source_roots", "source_occurrences", "source_members", "source_scans", "ingestion_jobs"} {
		var n int
		if err := db.Pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("reset retained %s: %d %v", table, n, err)
		}
	}
}
