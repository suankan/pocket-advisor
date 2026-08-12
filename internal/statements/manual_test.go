//go:build manual

package statements

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/workspace"
)

// Database-backed rehearsal of PostgresStore against the real schema.
// Manual because it needs PostgreSQL; normal tests exercise List's filtering,
// period, sorting, and truncation logic through the fake Store and remain
// hermetic (internal/mailbox follows the same split for the same reason).
//
// Run with a disposable database:
//
//	STATEMENTS_DSN=postgres://<role>@localhost:5432/<disposable-db> \
//		go test -tags manual ./internal/statements/ -run Manual -v
const manualWorkspace = "statements-manual"

func manualScratch(t *testing.T) *postgres.DB {
	t.Helper()
	dsn := os.Getenv("STATEMENTS_DSN")
	if dsn == "" {
		t.Skip("set STATEMENTS_DSN to a disposable database")
	}
	ctx := context.Background()
	admin, err := postgres.Connect(ctx, dsn, 2)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	schema := "statements_manual_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	if _, err := admin.Pool.Exec(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
		t.Fatalf("clear scratch schema: %v", err)
	}
	if _, err := admin.Pool.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create scratch schema: %v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema+",public")
	u.RawQuery = q.Encode()
	db, err := postgres.Connect(ctx, u.String(), 4)
	if err != nil {
		t.Fatalf("connect scratch schema: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		if _, err := admin.Pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
			t.Errorf("drop scratch schema: %v", err)
		}
		admin.Close()
	})
	if err := db.ApplySchema(ctx, postgres.SchemaMetadata{EmbedModel: "statements-manual", EmbedDim: 8}); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return db
}

func manualDocument(t *testing.T, db *postgres.DB, collectionID, filename, text string) {
	t.Helper()
	docID := domain.NewDocID(manualWorkspace, collectionID, filename)
	if _, err := db.Pool.Exec(context.Background(), `
        INSERT INTO documents (doc_id, workspace_id, collection_id, processing_status, doc_type, source_filename, normalized_text)
        VALUES ($1, $2, $3, 'COMPLETED', 'pdf', $4, $5)`,
		docID, manualWorkspace, collectionID, filename, text); err != nil {
		t.Fatalf("insert document: %v", err)
	}
}

func manualWorkspaceRegistry() *workspace.Resolved {
	return &workspace.Resolved{
		ID: manualWorkspace,
		Collections: []workspace.ResolvedCollection{
			{Collection: workspace.Collection{
				ID: "suan-cba-account", Title: "Suan CBA Complete Access",
				IngestionType: "bank-transactions", BSB: "062018", AccountNumber: "10321472", Owners: []string{"suan"},
			}},
		},
	}
}

func TestManualPostgresStoreReturnsCompletedDocumentsForResolvedCollections(t *testing.T) {
	db := manualScratch(t)
	ctx := context.Background()
	manualDocument(t, db, "suan-cba-account", "Statements20260118.pdf", "19 Jan 2026 OPENING BALANCE\n18 Apr 2026 CLOSING BALANCE")
	manualDocument(t, db, "suan-cba-account", "Statements20260418.pdf", "19 Apr 2026 OPENING BALANCE\n18 Jul 2026 CLOSING BALANCE")

	svc := New(NewPostgresStore(db), manualWorkspaceRegistry(), manualWorkspace)
	res, err := svc.List(ctx, Filters{Owner: "suan"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Packets) != 2 {
		t.Fatalf("got %d packets, want 2", len(res.Packets))
	}
	if res.Packets[0].Document.Title != "Statements20260118.pdf" {
		t.Errorf("first packet = %q, want the chronologically earlier statement", res.Packets[0].Document.Title)
	}
}

func TestManualPostgresStorePeriodFilterAgainstRealSchema(t *testing.T) {
	db := manualScratch(t)
	ctx := context.Background()
	manualDocument(t, db, "suan-cba-account", "in-period.pdf", "19 Apr 2026 OPENING BALANCE\n18 Jul 2026 CLOSING BALANCE")
	manualDocument(t, db, "suan-cba-account", "out-of-period.pdf", "19 Jan 2020 OPENING BALANCE\n18 Apr 2020 CLOSING BALANCE")

	svc := New(NewPostgresStore(db), manualWorkspaceRegistry(), manualWorkspace)
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	res, err := svc.List(ctx, Filters{Owner: "suan", Since: &since, Until: &until})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Packets) != 1 || res.Packets[0].Document.Title != "in-period.pdf" {
		t.Fatalf("got %+v, want only in-period.pdf", res.Packets)
	}
}
