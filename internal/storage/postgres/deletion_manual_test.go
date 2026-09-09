//go:build manual

package postgres

import (
	"context"
	"testing"

	"github.com/suankan/pocket-advisor/internal/domain"
)

func TestForgetRetainsSharedAndIndependentDescendants(t *testing.T) {
	db, _ := scratch(t, "forget_shared")
	ctx := context.Background()
	if err := db.ApplySchema(ctx, stageBMeta()); err != nil {
		t.Fatal(err)
	}
	r := NewDocumentRepo(db)
	first := stageDoc(t, r, "first", "email", "first.eml")
	second := stageDoc(t, r, "second", "email", "second.eml")
	shared := stageDoc(t, r, "shared", "archive", "")
	sharedChild := stageDoc(t, r, "shared-child", "text", "")
	exclusive := stageDoc(t, r, "exclusive", "archive", "")
	exclusiveChild := stageDoc(t, r, "exclusive-child", "text", "")
	independent := stageDoc(t, r, "independent", "text", "staged.txt")
	for _, p := range [][2]string{{first, shared}, {second, shared}, {shared, sharedChild}, {first, exclusive}, {exclusive, exclusiveChild}, {first, independent}, {second, independent}} {
		if err := r.RecordPlacement(ctx, p[0], p[1]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE documents SET parent_doc_id=$1 WHERE doc_id=$2`, first, shared); err != nil {
		t.Fatal(err)
	}
	n, err := r.DeleteBySHA(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("deleted %d, want selected container and two exclusive descendants", n)
	}
	for _, id := range []string{second, shared, sharedChild, independent} {
		if ok, err := r.Exists(ctx, id); err != nil || !ok {
			t.Fatalf("lost retained document: exists=%v err=%v", ok, err)
		}
	}
	var parent string
	if err := db.Pool.QueryRow(ctx, `SELECT parent_doc_id::text FROM documents WHERE doc_id=$1`, shared).Scan(&parent); err != nil {
		t.Fatal(err)
	}
	if parent != second {
		t.Fatal("shared provenance was not repointed")
	}
	var independentParent *string
	if err := db.Pool.QueryRow(ctx, `SELECT parent_doc_id::text FROM documents WHERE doc_id=$1`, independent).Scan(&independentParent); err != nil {
		t.Fatal(err)
	}
	if independentParent != nil {
		t.Fatal("independent root was reparented")
	}
	// A later deliberate scan may reintroduce a still-configured source.
	if _, _, err := r.CreateStub(ctx, &domain.Document{DocID: domain.NewDocID(), RawSHA256: "first"}); err != nil {
		t.Fatal(err)
	}
}

func TestFailedDocumentDeletionDoesNotQueueObjectRemoval(t *testing.T) {
	db, _ := scratch(t, "deletion_rollback")
	ctx := context.Background()
	if err := db.ApplySchema(ctx, stageBMeta()); err != nil {
		t.Fatal(err)
	}
	r := NewDocumentRepo(db)
	id := stageDoc(t, r, "retained", "text", "retained.txt")
	if _, err := db.Pool.Exec(ctx, `CREATE TABLE deletion_blocker (doc_id UUID REFERENCES documents(doc_id));
        INSERT INTO deletion_blocker SELECT doc_id FROM documents`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.DeleteDocuments(ctx, []string{id}); err == nil {
		t.Fatal("expected foreign key failure")
	}
	if ok, err := r.Exists(ctx, id); err != nil || !ok {
		t.Fatalf("exists=%v err=%v", ok, err)
	}
}
