//go:build manual

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/suankan/pocket-advisor/internal/source"
)

func retentionFixture(t *testing.T) (*DB, *DocumentRepo, *SourceRepo) {
	t.Helper()
	db, _ := scratch(t, t.Name())
	if err := db.ApplySchema(context.Background(), stageBMeta()); err != nil {
		t.Fatal(err)
	}
	unlock, err := db.LockMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
	return db, NewDocumentRepo(db), NewSourceRepo(db)
}

func observeScan(t *testing.T, inv *SourceRepo, documents ...string) SourceScan {
	t.Helper()
	ctx := context.Background()
	scan, err := inv.StartScan(ctx, []string{"synthetic-root"})
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range documents {
		if err := inv.Observe(ctx, scan.ID(), id, source.File{RootKey: "synthetic-root",
			RelativePath: id, DisplayPath: id, ModifiedAt: time.Now(), Size: int64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	scan, err = inv.CompleteScan(ctx, scan)
	if err != nil {
		t.Fatal(err)
	}
	return scan
}

func TestScanRetentionSharedNestedAndIndependent(t *testing.T) {
	_, docs, inv := retentionFixture(t)
	ctx := context.Background()
	a := stageDoc(t, docs, "a", "email", "a.eml")
	b := stageDoc(t, docs, "b", "email", "b.eml")
	nested := stageDoc(t, docs, "nested", "archive", "")
	shared := stageDoc(t, docs, "shared", "pdf", "")
	unheld := stageDoc(t, docs, "unheld", "pdf", "")
	independent := stageDoc(t, docs, "independent", "pdf", "independent.pdf")
	for _, edge := range [][2]string{{a, nested}, {nested, shared}, {b, shared}, {b, unheld}, {b, independent}} {
		if err := docs.RecordPlacement(ctx, edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{a, b, nested} {
		if err := docs.MarkContainmentEstablished(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	// A failed status does not erase a proved containment graph.
	if _, err := docs.db.Pool.Exec(ctx, `UPDATE documents SET processing_status='FAILED' WHERE doc_id=$1`, a); err != nil {
		t.Fatal(err)
	}
	scan := observeScan(t, inv, a, independent)
	got, err := docs.RetentionForScan(ctx, scan)
	if err != nil {
		t.Fatal(err)
	}
	removed := ids(got.Candidates)
	if len(removed) != 2 || !removed[b] || !removed[unheld] || got.UnestablishedContainers != 0 {
		t.Fatalf("wrong retention: %+v", got)
	}
	if _, err := docs.DeleteDocuments(ctx, []string{b, unheld}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a, nested, shared, independent} {
		if exists, err := docs.Exists(ctx, id); err != nil || !exists {
			t.Fatalf("lost retained document: %v", err)
		}
	}
}

func TestScanRetentionRequiresNestedContainmentProof(t *testing.T) {
	_, docs, inv := retentionFixture(t)
	ctx := context.Background()
	root := stageDoc(t, docs, "root", "email", "")
	child := stageDoc(t, docs, "child", "archive", "")
	if err := docs.RecordPlacement(ctx, root, child); err != nil {
		t.Fatal(err)
	}
	if err := docs.MarkContainmentEstablished(ctx, root); err != nil {
		t.Fatal(err)
	}
	scan := observeScan(t, inv, root)
	got, err := docs.RetentionForScan(ctx, scan)
	if err != nil || got.UnestablishedContainers != 1 {
		t.Fatalf("nested gap missed: %+v %v", got, err)
	}
	if err := docs.MarkContainmentEstablished(ctx, child); err != nil {
		t.Fatal(err)
	}
	got, err = docs.RetentionForScan(ctx, scan)
	if err != nil || got.UnestablishedContainers != 0 {
		t.Fatalf("established graph refused: %+v %v", got, err)
	}
	// Explicitly forgetting a child must invalidate its surviving parent's proof.
	if _, err := docs.DeleteDocuments(ctx, []string{child}); err != nil {
		t.Fatal(err)
	}
	got, err = docs.RetentionForScan(ctx, scan)
	if err != nil || got.UnestablishedContainers != 1 {
		t.Fatalf("deleted edge retained proof: %+v %v", got, err)
	}
}

func TestScanRetentionRejectsPartialSupersededAndForeignOperations(t *testing.T) {
	db, docs, inv := retentionFixture(t)
	ctx := context.Background()
	id := stageDoc(t, docs, "root", "pdf", "")
	first := observeScan(t, inv, id)
	partial, err := inv.StartScan(ctx, []string{"synthetic-root"})
	if err != nil {
		t.Fatal(err)
	}
	for _, scan := range []SourceScan{first, partial, {}} {
		if _, err := docs.RetentionForScan(ctx, scan); !errors.Is(err, ErrInvalidRetentionScan) {
			t.Fatalf("invalid scan accepted: %v", err)
		}
	}
	current := observeScan(t, inv, id)
	other := &DocumentRepo{db: &DB{Pool: db.Pool}}
	if _, err := other.RetentionForScan(ctx, current); !errors.Is(err, ErrInvalidRetentionScan) {
		t.Fatalf("foreign operation accepted: %v", err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE source_scans SET root_keys=ARRAY['different'] WHERE scan_id=$1`, current.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := docs.RetentionForScan(ctx, current); !errors.Is(err, ErrInvalidRetentionScan) {
		t.Fatalf("wrong roots accepted: %v", err)
	}
}

func TestContainmentProofRollsBackWithPlacements(t *testing.T) {
	db, docs, _ := retentionFixture(t)
	ctx := context.Background()
	root := stageDoc(t, docs, "root", "email", "")
	child := stageDoc(t, docs, "child", "pdf", "")
	fault := errors.New("injected stage failure")
	err := db.Transaction(ctx, func(ctx context.Context) error {
		if err := docs.RecordPlacement(ctx, root, child); err != nil {
			return err
		}
		if err := docs.MarkContainmentEstablished(ctx, root); err != nil {
			return err
		}
		return fault
	})
	if !errors.Is(err, fault) {
		t.Fatal(err)
	}
	var established bool
	if err := db.Pool.QueryRow(ctx, `SELECT containment_established_at IS NOT NULL FROM documents WHERE doc_id=$1`, root).Scan(&established); err != nil {
		t.Fatal(err)
	}
	has, err := docs.HasPlacements(ctx, root)
	if err != nil || has || established {
		t.Fatalf("partial stage escaped: placements=%v proof=%v err=%v", has, established, err)
	}
}
