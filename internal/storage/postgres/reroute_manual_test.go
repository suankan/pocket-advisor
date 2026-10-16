//go:build manual

package postgres

import (
	"context"
	"testing"

	"github.com/suankan/pocket-advisor/internal/domain"
)

// Content misread as plain text is turned back into a pending document of its
// real type: its text, passages and jobs go, a passage another document still
// holds stays, and a document a handler is running on is left alone.
func TestRerouteResetsMisreadText(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	chunks := NewChunkRepo(db)
	vec := make([]float32, stageBDim)
	vec[0] = 1

	misread, _ := addJob(t, db, docs, jobs, SubjectEmbed)
	other, _ := addJob(t, db, docs, jobs, SubjectEmbed)
	for _, id := range []string{misread, other} {
		if err := docs.SaveText(ctx, id, "Received: raw source\nshared line", "text"); err != nil {
			t.Fatal(err)
		}
		if err := jobs.Run(ctx, claimOne(t, jobs, SubjectEmbed), func(ctx context.Context) error {
			return chunks.ReplaceChunks(ctx, id, []domain.Chunk{
				{ChunkID: domain.NewChunkID(), DocID: id, Text: "shared line", EmbedModel: "m", Embedding: vec},
				{ChunkID: domain.NewChunkID(), DocID: id, Text: "only in " + id, EmbedModel: "m", Embedding: vec},
			})
		}); err != nil {
			t.Fatal(err)
		}
	}
	before := count(t, db, `SELECT count(*) FROM chunks`)

	var rerouted bool
	if err := db.Transaction(ctx, func(ctx context.Context) error {
		var err error
		rerouted, err = docs.Reroute(ctx, misread, "email", "message/rfc822")
		return err
	}); err != nil || !rerouted {
		t.Fatalf("rerouted %v, err %v", rerouted, err)
	}
	status, docType, err := docs.KnownRoute(ctx, misread)
	if err != nil || status != domain.StatusPending || docType != "email" {
		t.Fatalf("after reroute: %s %q %v", status, docType, err)
	}
	if n := count(t, db, `SELECT count(*) FROM ingestion_jobs WHERE doc_id = $1`, misread); n != 0 {
		t.Errorf("%d jobs left", n)
	}
	if n := count(t, db, `SELECT count(*) FROM document_chunks WHERE doc_id = $1`, misread); n != 0 {
		t.Errorf("%d placements left", n)
	}
	if n := count(t, db, `SELECT count(*) FROM documents WHERE doc_id = $1 AND normalized_text IS NULL`, misread); n != 1 {
		t.Error("its text was kept")
	}
	if after := count(t, db, `SELECT count(*) FROM chunks`); after != before-1 {
		t.Errorf("passages %d → %d; want only its own to go, the shared one kept", before, after)
	}

	// A running job keeps the document as it is.
	running, _ := addJob(t, db, docs, jobs, SubjectEmbed)
	claimOne(t, jobs, SubjectEmbed)
	if err := db.Transaction(ctx, func(ctx context.Context) error {
		var err error
		rerouted, err = docs.Reroute(ctx, running, "email", "message/rfc822")
		return err
	}); err != nil || rerouted {
		t.Fatalf("a document with a running job: rerouted %v, err %v", rerouted, err)
	}
}

func count(t *testing.T, db *DB, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
