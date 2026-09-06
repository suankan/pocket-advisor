//go:build manual

package postgres

import (
	"context"
	"testing"
)

// The live workspace is already provisioned, so this DDL reaches it only
// through the upgrade path. This rehearses that path against the old shape:
// documents with ON DELETE CASCADE, rows already present, no placements table.
func TestPlacementUpgradeFromAnAlreadyProvisionedWorkspace(t *testing.T) {
	db, _ := scratch(t, "placement_upgrade")
	defer db.Close()
	ctx := context.Background()

	// Provision at the old shape, then prove the new objects are absent.
	if err := db.ApplySchema(ctx, stageBMeta()); err != nil {
		t.Fatalf("initial apply: %v", err)
	}
	repo := &DocumentRepo{db: db}
	parent := stageDoc(t, repo, "pppp", "email", "mail/p.eml")
	child := stageDoc(t, repo, "cccc", "pdf", "")
	if _, err := db.Pool.Exec(ctx, `DROP TABLE IF EXISTS document_placements, pending_object_deletions`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `ALTER TABLE schema_metadata DROP COLUMN IF EXISTS placements_backfilled_at`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `
        ALTER TABLE documents DROP CONSTRAINT documents_parent_doc_id_fkey;
        ALTER TABLE documents ADD CONSTRAINT documents_parent_doc_id_fkey
            FOREIGN KEY (parent_doc_id) REFERENCES documents(doc_id) ON DELETE CASCADE`); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Pool.Exec(ctx,
		`UPDATE documents SET parent_doc_id = $1 WHERE doc_id = $2`, parent, child); err != nil {
		t.Fatal(err)
	}

	// The upgrade path, as an already-provisioned workspace reaches it.
	if err := db.ApplyPlacementSchema(ctx); err != nil {
		t.Fatalf("upgrade apply: %v", err)
	}

	var delType string
	if err := db.Pool.QueryRow(ctx, `
        SELECT confdeltype::text FROM pg_constraint
        WHERE conname = 'documents_parent_doc_id_fkey'
          AND conrelid = 'documents'::regclass`).Scan(&delType); err != nil {
		t.Fatal(err)
	}
	if delType != "n" {
		t.Errorf("parent FK delete action is %q, want SET NULL; CASCADE would delete shared attachments", delType)
	}

	var seeded int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM document_placements WHERE container_doc_id = $1 AND contained_doc_id = $2`,
		parent, child).Scan(&seeded); err != nil {
		t.Fatal(err)
	}
	if seeded != 1 {
		t.Errorf("existing parent link was not seeded into placements (got %d)", seeded)
	}

	// The marker must stay unset, so pruning refuses until the backfill runs.
	at, err := repo.PlacementsBackfilledAt(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !at.IsZero() {
		t.Error("upgrade marked containment backfilled; pruning would sweep the seeded floor")
	}

	// Applying twice must be a no-op, not an error.
	if err := db.ApplySchema(ctx, stageBMeta()); err != nil {
		t.Fatalf("second upgrade apply: %v", err)
	}
}
