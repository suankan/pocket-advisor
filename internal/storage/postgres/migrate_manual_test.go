//go:build manual

package postgres

import (
	"context"
	"os"
	"testing"
)

// TestMigrateChunkContent rehearses the deduplication migration against a
// throwaway clone of a real database, which is the only way to know it holds
// against real data shapes without risking a workspace.
//
//	Run: MIGRATE_DSN=postgres://.../dedup_clone go test -tags manual \
//		./internal/storage/postgres/ -run MigrateChunk -v
func TestMigrateChunkContent(t *testing.T) {
	dsn := os.Getenv("MIGRATE_DSN")
	if dsn == "" {
		t.Skip("set MIGRATE_DSN to a disposable clone")
	}
	ctx := context.Background()
	db, err := Connect(ctx, dsn, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var beforeRows, beforeDistinct int
	if err := db.Pool.QueryRow(ctx, `
        SELECT count(*),
               count(DISTINCT sha256(convert_to(btrim(regexp_replace(chunk_text,'\s+',' ','g')),'UTF8')))
        FROM document_chunks`).Scan(&beforeRows, &beforeDistinct); err != nil {
		t.Fatal(err)
	}
	t.Logf("before: %d placements, %d distinct passages", beforeRows, beforeDistinct)

	meta, err := db.LoadSchemaMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.migrateChunkContent(ctx, meta.EmbedDim); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var placements, passages int
	if err := db.Pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM document_chunks), (SELECT count(*) FROM chunks)`,
	).Scan(&placements, &passages); err != nil {
		t.Fatal(err)
	}
	t.Logf("after:  %d placements, %d passages (%d rows saved, %.1f%%)",
		placements, passages, beforeRows-passages,
		100*float64(beforeRows-passages)/float64(beforeRows))

	if placements != beforeRows {
		t.Errorf("placements = %d, want %d: no document may lose a chunk", placements, beforeRows)
	}
	if passages != beforeDistinct {
		t.Errorf("passages = %d, want %d distinct", passages, beforeDistinct)
	}

	// Every placement must resolve to text and a vector.
	var orphans, nullVecs int
	if err := db.Pool.QueryRow(ctx, `
        SELECT (SELECT count(*) FROM document_chunks p
                WHERE NOT EXISTS (SELECT 1 FROM chunks c WHERE c.content_id = p.content_id)),
               (SELECT count(*) FROM chunks WHERE embedding IS NULL)`,
	).Scan(&orphans, &nullVecs); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Errorf("%d placements point at no passage", orphans)
	}
	if nullVecs != 0 {
		t.Errorf("%d passages lost their embedding", nullVecs)
	}

	// Idempotent: a second run must be a no-op, not an error.
	if err := db.migrateChunkContent(ctx, meta.EmbedDim); err != nil {
		t.Fatalf("second run: %v", err)
	}
	var again int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM chunks`).Scan(&again); err != nil {
		t.Fatal(err)
	}
	if again != passages {
		t.Errorf("second run changed passages %d -> %d", passages, again)
	}
}
