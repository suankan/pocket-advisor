//go:build manual

package postgres

import (
	"context"
	"testing"

	"github.com/suankan/pocket-advisor/internal/domain"
)

// Database-backed rehearsal of containment and the sweep that reads it
// (ingestion-design.md §6.5). Manual because the reachability query is SQL and
// only a real PostgreSQL can answer whether it is right.
//
//	Run: EMAIL_DSN=postgres://<role>@localhost:5432/<disposable-db> \
//		go test -tags manual ./internal/storage/postgres/ -run Placement -v
//
// Every document here is synthetic; nothing reads a workspace.

// stageDoc inserts one document directly, standing in for whatever discovery
// or the email worker would have created.
func stageDoc(t *testing.T, r *DocumentRepo, sha, docType, sourcePath string) string {
	t.Helper()
	d := &domain.Document{
		DocID:      domain.NewDocID(),
		DocType:    docType,
		RawSHA256:  sha,
		SourceName: sha + ".bin",
	}
	if sourcePath != "" {
		d.Metadata = map[string]string{"source_path": sourcePath}
	}
	id, _, err := r.CreateStub(context.Background(), d)
	if err != nil {
		t.Fatalf("stage %s: %v", sha, err)
	}
	return id
}

func ids(cs []PruneCandidate) map[string]bool {
	out := make(map[string]bool, len(cs))
	for _, c := range cs {
		out[c.DocID] = true
	}
	return out
}

// The case the placements table exists for.
//
// EmailA carries DocA, DocB, DocC. EmailB carries DocA, DocB, DocD. Content
// identity is unique per workspace, so DocA and DocB are each one row, and
// under parent_doc_id alone they belong to whichever email was processed
// first. Ignoring EmailB must take DocD and nothing else: EmailA still carries
// the other two, and the point is that this holds regardless of which email
// happened to win the race to create them.
func TestPlacementKeepsAttachmentsSharedWithASurvivingEmail(t *testing.T) {
	db, _ := scratch(t, "placement_shared")
	defer db.Close()
	if err := db.ApplySchema(context.Background(), stageBMeta()); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	ctx := context.Background()
	repo := &DocumentRepo{db: db}

	emailA := stageDoc(t, repo, "aaaa", "email", "mail/a.eml")
	emailB := stageDoc(t, repo, "bbbb", "email", "mail/b.eml")
	docA := stageDoc(t, repo, "1111", "pdf", "")
	docB := stageDoc(t, repo, "2222", "pdf", "")
	docC := stageDoc(t, repo, "3333", "pdf", "")
	docD := stageDoc(t, repo, "4444", "pdf", "")

	// EmailB is deliberately recorded first for DocA and DocB, which is the
	// ordering that used to lose them.
	for _, p := range [][2]string{
		{emailB, docA}, {emailB, docB}, {emailB, docD},
		{emailA, docA}, {emailA, docB}, {emailA, docC},
	} {
		if err := repo.RecordPlacement(ctx, p[0], p[1]); err != nil {
			t.Fatalf("record placement: %v", err)
		}
	}

	// EmailB's own file is no longer staged; EmailA's still is.
	got, err := repo.UnreachableDocuments(ctx, []string{"aaaa"})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	unreachable := ids(got)

	for name, id := range map[string]string{"DocA": docA, "DocB": docB, "DocC": docC, "EmailA": emailA} {
		if unreachable[id] {
			t.Errorf("%s was swept although EmailA still carries it", name)
		}
	}
	for name, id := range map[string]string{"EmailB": emailB, "DocD": docD} {
		if !unreachable[id] {
			t.Errorf("%s survived although nothing stages or carries it", name)
		}
	}
}

// Containment nests, so removing a container orphans a subtree rather than a
// row. A single pass over direct references would leave the tail behind.
func TestPlacementSweepsNestedContainmentTransitively(t *testing.T) {
	db, _ := scratch(t, "placement_nested")
	defer db.Close()
	if err := db.ApplySchema(context.Background(), stageBMeta()); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	ctx := context.Background()
	repo := &DocumentRepo{db: db}

	email := stageDoc(t, repo, "eeee", "email", "mail/e.eml")
	archive := stageDoc(t, repo, "zzzz", "archive", "")
	inner := stageDoc(t, repo, "iiii", "pdf", "")
	deepest := stageDoc(t, repo, "dddd", "pdf", "")

	for _, p := range [][2]string{{email, archive}, {archive, inner}, {inner, deepest}} {
		if err := repo.RecordPlacement(ctx, p[0], p[1]); err != nil {
			t.Fatal(err)
		}
	}

	// Still staged: everything stays.
	got, err := repo.UnreachableDocuments(ctx, []string{"eeee"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("%d document(s) swept while the email is still staged", len(got))
	}

	// No longer staged: the whole chain goes, not just the first link.
	got, err = repo.UnreachableDocuments(ctx, []string{"unrelated"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Errorf("swept %d of 4; a nested subtree must go with its container", len(got))
	}
}

// Deleting a container must not take what it contains, and a survivor must not
// be left unable to say where it came from.
func TestDeleteDocumentsRepointsSurvivingProvenance(t *testing.T) {
	db, _ := scratch(t, "placement_repoint")
	defer db.Close()
	if err := db.ApplySchema(context.Background(), stageBMeta()); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	ctx := context.Background()
	repo := &DocumentRepo{db: db}

	emailA := stageDoc(t, repo, "aaaa", "email", "mail/a.eml")
	emailB := stageDoc(t, repo, "bbbb", "email", "mail/b.eml")
	shared := stageDoc(t, repo, "1111", "pdf", "")

	// The shared attachment's provenance points at the email about to go.
	if _, err := db.Pool.Exec(ctx,
		`UPDATE documents SET parent_doc_id = $1 WHERE doc_id = $2`, emailB, shared); err != nil {
		t.Fatal(err)
	}
	for _, p := range [][2]string{{emailB, shared}, {emailA, shared}} {
		if err := repo.RecordPlacement(ctx, p[0], p[1]); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := repo.DeleteDocuments(ctx, []string{emailB}); err != nil {
		t.Fatalf("delete: %v", err)
	}

	var parent *string
	if err := db.Pool.QueryRow(ctx,
		`SELECT parent_doc_id::text FROM documents WHERE doc_id = $1`, shared).Scan(&parent); err != nil {
		t.Fatalf("the shared attachment was deleted with its container: %v", err)
	}
	if parent == nil {
		t.Fatal("survivor kept a null parent; it can no longer say where it came from")
	}
	if *parent != emailA {
		t.Errorf("survivor repointed at %s, want the surviving container %s", *parent, emailA)
	}
}

// Repeat extraction is normal — a redelivered email re-records what it holds —
// so a placement that already exists is success.
func TestRecordPlacementIsIdempotent(t *testing.T) {
	db, _ := scratch(t, "placement_idempotent")
	defer db.Close()
	if err := db.ApplySchema(context.Background(), stageBMeta()); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	ctx := context.Background()
	repo := &DocumentRepo{db: db}

	parent := stageDoc(t, repo, "pppp", "email", "mail/p.eml")
	child := stageDoc(t, repo, "cccc", "pdf", "")

	for i := 0; i < 3; i++ {
		if err := repo.RecordPlacement(ctx, parent, child); err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	var n int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM document_placements WHERE container_doc_id = $1`, parent).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d placement rows after three identical records, want 1", n)
	}
}
