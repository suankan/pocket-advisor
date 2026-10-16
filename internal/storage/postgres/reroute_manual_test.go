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
	k, err := docs.Known(ctx, misread)
	if err != nil || k.Status != domain.StatusPending || k.DocType != "email" || !k.Jobless {
		t.Fatalf("after reroute: %+v %v", k, err)
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

// A document no handler reads any more is recorded as unsupported: its jobs,
// text and passages go, and so does what it held as a container, while the
// members themselves stay for the sweep to judge.
func TestRefuseRecordsUnsupportedAndReleasesMembers(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	chunks := NewChunkRepo(db)
	vec := make([]float32, stageBDim)
	vec[0] = 1

	book, _ := addJob(t, db, docs, jobs, SubjectEmbed)
	if k, err := docs.Known(ctx, book); err != nil || k.Jobless {
		t.Fatalf("a document with a job read as jobless: %+v %v", k, err)
	}
	chapter, _ := addJob(t, db, docs, jobs, SubjectEmbed)
	if err := docs.SaveText(ctx, book, "chapter one", "text"); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Run(ctx, claimOne(t, jobs, SubjectEmbed), func(ctx context.Context) error {
		return chunks.ReplaceChunks(ctx, book, []domain.Chunk{{ChunkID: domain.NewChunkID(), DocID: book, Text: "chapter one", EmbedModel: "m", Embedding: vec}})
	}); err != nil {
		t.Fatal(err)
	}
	if err := docs.RecordPlacement(ctx, book, chapter); err != nil {
		t.Fatal(err)
	}
	if err := NewSourceRepo(db).RecordMember(ctx, book, chapter, "zip:OEBPS/1.xhtml"); err != nil {
		t.Fatal(err)
	}

	var refused bool
	if err := db.Transaction(ctx, func(ctx context.Context) error {
		var err error
		refused, err = docs.Refuse(ctx, book, "application/epub+zip")
		return err
	}); err != nil || !refused {
		t.Fatalf("refused %v, err %v", refused, err)
	}
	k, err := docs.Known(ctx, book)
	if err != nil || k.Status != domain.StatusSkipped || k.Reason != domain.ReasonUnsupportedFormat || k.DocType != "" || !k.Jobless {
		t.Fatalf("after refuse: %+v %v", k, err)
	}
	for what, sql := range map[string]string{
		"passages":   `SELECT count(*) FROM document_chunks WHERE doc_id = $1`,
		"placements": `SELECT count(*) FROM document_placements WHERE container_doc_id = $1`,
		"members":    `SELECT count(*) FROM source_members WHERE parent_id = $1`,
		"text":       `SELECT count(*) FROM documents WHERE doc_id = $1 AND normalized_text IS NOT NULL`,
	} {
		if n := count(t, db, sql, book); n != 0 {
			t.Errorf("%d %s left", n, what)
		}
	}
	if n := count(t, db, `SELECT count(*) FROM documents WHERE doc_id = $1`, chapter); n != 1 {
		t.Error("the member itself was deleted; only the sweep deletes documents")
	}
}
