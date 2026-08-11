//go:build manual

package postgres

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/suankan/pocket-advisor/internal/domain"
)

// Database-backed rehearsal of the selection query behind email metadata
// reprocessing (ingestion-design.md §2.5). Manual for the same reason as the
// rest of this file's suite: it needs a live PostgreSQL.
//
//	Run: EMAIL_DSN=postgres://<role>@localhost:5432/<disposable-db> \
//		go test -tags manual ./internal/storage/postgres/ -run EmailDocuments -v
//
// All content is synthetic. Nothing here reads a workspace.

// reprocessDoc creates one Tier 2 row the way discovery would have, with the
// doc_type and Tier 1 URI the selection query filters on.
func reprocessDoc(t *testing.T, db *DB, workspace, sha, docType, rawURI string) string {
	t.Helper()
	docID := domain.NewDocID(workspace, "stage-b", sha)
	if _, err := db.Pool.Exec(context.Background(), `
        INSERT INTO documents (doc_id, workspace_id, collection_id, processing_status,
                               doc_type, mime_type, rustfs_raw_uri, raw_sha256)
        VALUES ($1, $2, 'stage-b', 'COMPLETED', $3, 'message/rfc822', $4, $5)
        ON CONFLICT (doc_id) DO NOTHING`,
		docID, workspace, docType, rawURI, sha); err != nil {
		t.Fatalf("create document: %v", err)
	}
	return docID
}

func docIDs(docs []domain.Document) []string {
	out := make([]string, len(docs))
	for i, d := range docs {
		out[i] = d.DocID
	}
	return out
}

// A rebuild walks message documents and nothing else: an archive is a
// container rather than a message, a non-email document has no metadata to
// rebuild, and another workspace is never in scope at all. An email row with
// no Tier 1 object is deliberately selected: the command must report it as
// unreadable rather than silently skip the missing metadata.
func TestEmailDocumentsSelectsOnlyMessageDocuments(t *testing.T) {
	db, repo := applied(t, "select")
	ctx := context.Background()

	wantSet := map[string]bool{}
	for i, sha := range []string{strings.Repeat("a", 64), strings.Repeat("b", 64)} {
		wantSet[reprocessDoc(t, db, stageBWorkspace, sha, "email",
			fmt.Sprintf("s3://bucket/raw/%d", i))] = true
	}
	reprocessDoc(t, db, stageBWorkspace, strings.Repeat("c", 64), "archive", "s3://bucket/raw/c")
	reprocessDoc(t, db, stageBWorkspace, strings.Repeat("d", 64), "pdf", "s3://bucket/raw/d")
	missingObject := reprocessDoc(t, db, stageBWorkspace, strings.Repeat("e", 64), "email", "")
	wantSet[missingObject] = true
	reprocessDoc(t, db, "other-workspace", strings.Repeat("f", 64), "email", "s3://bucket/raw/f")

	got, err := repo.EmailDocuments(ctx, EmailDocumentQuery{
		WorkspaceID: stageBWorkspace, Limit: 100,
	})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(got) != len(wantSet) {
		t.Fatalf("selected %v, want the %d message documents", docIDs(got), len(wantSet))
	}
	for _, d := range got {
		if !wantSet[d.DocID] {
			t.Errorf("selected an out-of-scope document %s", d.DocID)
		}
		if d.WorkspaceID != stageBWorkspace {
			t.Errorf("document %s has workspace %q", d.DocID, d.WorkspaceID)
		}
	}
}

// The walk is a keyset over doc_id, which is what makes an interrupted rebuild
// resumable: the same corpus yields the same order, and the cursor is the last
// document the previous page returned.
func TestEmailDocumentsPagesDeterministically(t *testing.T) {
	db, repo := applied(t, "pages")
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		reprocessDoc(t, db, stageBWorkspace, strings.Repeat(fmt.Sprint(i), 64),
			"email", fmt.Sprintf("s3://bucket/raw/%d", i))
	}

	all, err := repo.EmailDocuments(ctx, EmailDocumentQuery{WorkspaceID: stageBWorkspace, Limit: 100})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("selected %d documents, want 5", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].DocID >= all[i].DocID {
			t.Fatalf("order is not ascending by doc_id: %v", docIDs(all))
		}
	}

	var paged []string
	cursor := ""
	for {
		page, err := repo.EmailDocuments(ctx, EmailDocumentQuery{
			WorkspaceID: stageBWorkspace, After: cursor, Limit: 2,
		})
		if err != nil {
			t.Fatalf("page after %q: %v", cursor, err)
		}
		if len(page) == 0 {
			break
		}
		paged = append(paged, docIDs(page)...)
		cursor = page[len(page)-1].DocID
	}
	if strings.Join(paged, ",") != strings.Join(docIDs(all), ",") {
		t.Errorf("paged walk %v differs from the single-page walk %v", paged, docIDs(all))
	}
}

// The cheap pass after an interrupted rebuild: only what has no metadata row
// yet. A full pass rewrites everything and converges on the same rows, so this
// is a narrowing of work, never of correctness.
func TestEmailDocumentsCanSelectOnlyMissingMetadata(t *testing.T) {
	db, repo := applied(t, "missing")
	ctx := context.Background()

	done := reprocessDoc(t, db, stageBWorkspace, strings.Repeat("7", 64), "email", "s3://bucket/raw/7")
	pending := reprocessDoc(t, db, stageBWorkspace, strings.Repeat("8", 64), "email", "s3://bucket/raw/8")

	if _, err := repo.SaveEmailMessage(ctx,
		message(done, "seven@mail.example.test", "Quarterly review", "")); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := repo.EmailDocuments(ctx, EmailDocumentQuery{
		WorkspaceID: stageBWorkspace, Limit: 100, OnlyMissing: true,
	})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(got) != 1 || got[0].DocID != pending {
		t.Fatalf("selected %v, want only the document without metadata", docIDs(got))
	}

	// And the full pass still sees both, because a rebuild from Tier 1 is
	// allowed to rewrite a row that already exists.
	all, err := repo.EmailDocuments(ctx, EmailDocumentQuery{
		WorkspaceID: stageBWorkspace, Limit: 100,
	})
	if err != nil {
		t.Fatalf("select all: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("full pass selected %v, want both documents", docIDs(all))
	}
}

// Rebuilding metadata is not re-ingestion: the canonical document row, its
// extracted text, its thread id and its status belong to the pipeline, and a
// maintenance rewrite of the browse tables must leave every one of them alone.
func TestEmailReprocessingLeavesTheDocumentRowAlone(t *testing.T) {
	db, repo := applied(t, "canonical")
	ctx := context.Background()

	docID := reprocessDoc(t, db, stageBWorkspace, strings.Repeat("9", 64), "email", "s3://bucket/raw/9")
	if _, err := db.Pool.Exec(ctx, `
        UPDATE documents SET normalized_text = $2, thread_id = $3, email_subject = $4
        WHERE doc_id = $1`, docID, "synthetic body", "thread-key", "Quarterly review"); err != nil {
		t.Fatalf("seed document: %v", err)
	}

	type row struct {
		text, thread, subject, status string
		updated                       time.Time
	}
	read := func() row {
		t.Helper()
		var r row
		if err := db.Pool.QueryRow(ctx, `
            SELECT COALESCE(normalized_text,''), thread_id, email_subject,
                   processing_status::text, updated_at
            FROM documents WHERE doc_id = $1`, docID).Scan(
			&r.text, &r.thread, &r.subject, &r.status, &r.updated); err != nil {
			t.Fatalf("read document: %v", err)
		}
		return r
	}

	before := read()
	m := message(docID, "nine@mail.example.test", "Quarterly review", "")
	if _, err := repo.SaveEmailMessage(ctx, m); err != nil {
		t.Fatalf("first rebuild: %v", err)
	}
	if _, err := repo.SaveEmailMessage(ctx, m); err != nil {
		t.Fatalf("second rebuild: %v", err)
	}
	if after := read(); after != before {
		t.Errorf("rebuild changed the document row: %+v then %+v", before, after)
	}

	var chunks int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM document_chunks WHERE doc_id = $1`, docID).Scan(&chunks); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if chunks != 0 {
		t.Errorf("rebuild produced %d chunk rows, want none", chunks)
	}
}
