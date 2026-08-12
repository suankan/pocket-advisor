//go:build manual

package mailbox

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
)

// Database-backed rehearsal of the exact browse path. Manual because it needs
// PostgreSQL with pgvector; normal tests exercise all decisions through the
// fake Store and remain hermetic.
//
// Run with a disposable database:
//
//	EMAIL_DSN=postgres://<role>@localhost:5432/<disposable-db> \
//		go test -tags manual ./internal/mailbox/ -run Mailbox -v
//
// This test creates and drops one scratch schema. All content and addresses are
// synthetic .test values.
const manualWorkspace = "mailbox-manual"

func manualScratch(t *testing.T) (*postgres.DB, *postgres.EmailRepo) {
	t.Helper()
	dsn := os.Getenv("EMAIL_DSN")
	if dsn == "" {
		t.Skip("set EMAIL_DSN to a disposable database")
	}
	ctx := context.Background()
	admin, err := postgres.Connect(ctx, dsn, 2)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	schema := "mailbox_manual_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
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
	if err := db.ApplySchema(ctx, postgres.SchemaMetadata{EmbedModel: "mailbox-manual", EmbedDim: 8}); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return db, postgres.NewEmailRepo(db)
}

func manualMessage(t *testing.T, db *postgres.DB, n int, sender string, sent time.Time) domain.EmailMessage {
	t.Helper()
	docID := domain.NewDocID(manualWorkspace, fmt.Sprintf("%064x", n))
	if _, err := db.Pool.Exec(context.Background(), `
        INSERT INTO documents (doc_id, workspace_id, processing_status, doc_type)
        VALUES ($1, $2, 'COMPLETED', 'email')`, docID, manualWorkspace); err != nil {
		t.Fatalf("insert document: %v", err)
	}
	return domain.EmailMessage{
		DocID: docID, WorkspaceID: manualWorkspace,
		MessageID:  fmt.Sprintf("m%d@mail.example.test", n),
		SubjectRaw: "Synthetic mailbox message", SubjectNormalized: "synthetic mailbox message",
		SentAt: sent,
		Addresses: []domain.EmailAddress{
			{Kind: domain.EmailAddressFrom, Ordinal: 0, Address: sender, Raw: sender, Valid: true},
			{Kind: domain.EmailAddressTo, Ordinal: 0, Address: "owner@example.test", Raw: "owner@example.test", Valid: true},
		},
	}
}

func manualService(t *testing.T, db *postgres.DB) *Service {
	t.Helper()
	s, err := New(NewPostgresStore(db), manualWorkspace, Config{DefaultLimit: 2, MaxLimit: 10, MaxParticipants: 3}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMailboxManualFiltersOrderingAndStablePagination(t *testing.T) {
	db, repo := manualScratch(t)
	ctx := context.Background()
	old := manualMessage(t, db, 1, "ada@example.test", time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC))
	middle := manualMessage(t, db, 2, "ada@example.test", time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC))
	other := manualMessage(t, db, 3, "bob@other.example.test", time.Date(2026, 1, 3, 9, 0, 0, 0, time.UTC))
	// Exact recipient matching covers every recipient header, not only To.
	other.Addresses[1].Kind = domain.EmailAddressCc
	undated := manualMessage(t, db, 4, "ada@example.test", time.Time{})
	for _, m := range []domain.EmailMessage{old, middle, other, undated} {
		if _, err := repo.SaveEmailMessage(ctx, m); err != nil {
			t.Fatalf("save message: %v", err)
		}
	}

	s := manualService(t, db)
	page1, err := s.ListMessages(ctx, ListRequest{Sender: "Ada <ADA@EXAMPLE.TEST>", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page1.Messages) != 2 || page1.Messages[0].DocID != middle.DocID || page1.Messages[1].DocID != old.DocID {
		t.Fatalf("first page = %#v", page1.Messages)
	}
	if !page1.Page.HasMore || page1.Page.NextCursor == "" {
		t.Fatalf("page state = %#v", page1.Page)
	}

	// This backfill sorts between the already returned rows but was ingested
	// after the page's watermark. It must not move page 2; a fresh series sees it.
	backfill := manualMessage(t, db, 5, "ada@example.test", time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	if _, err := repo.SaveEmailMessage(ctx, backfill); err != nil {
		t.Fatal(err)
	}
	page2, err := s.ListMessages(ctx, ListRequest{Sender: "ada@example.test", Limit: 2, Cursor: page1.Page.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(page2.Messages) != 1 || page2.Messages[0].DocID != undated.DocID {
		t.Fatalf("stable second page = %#v", page2.Messages)
	}
	fresh, err := s.ListMessages(ctx, ListRequest{Sender: "ada@example.test", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh.Messages) != 4 || fresh.Messages[1].DocID != backfill.DocID {
		t.Fatalf("fresh browse = %#v", fresh.Messages)
	}
	// A domain is exact, not a suffix: the related subdomain sender above is
	// excluded while every exact example.test sender remains.
	domain, err := s.ListMessages(ctx, ListRequest{Sender: "EXAMPLE.TEST", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if domain.Filters.SenderDomain != "example.test" || len(domain.Messages) != 4 {
		t.Fatalf("domain browse = %#v", domain)
	}
	for _, message := range domain.Messages {
		if message.DocID == other.DocID {
			t.Fatalf("suffix sender appeared in exact domain browse: %#v", domain.Messages)
		}
	}
	recipients, err := s.ListMessages(ctx, ListRequest{Recipient: "owner@example.test", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(recipients.Messages) != 5 {
		t.Fatalf("recipient browse = %#v", recipients.Messages)
	}
	dated, err := s.ListMessages(ctx, ListRequest{
		After:  time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		Before: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC), Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dated.Messages) != 1 || dated.Messages[0].DocID != middle.DocID {
		t.Fatalf("date browse = %#v", dated.Messages)
	}

	oldest, err := s.ListMessages(ctx, ListRequest{Order: OrderOldestFirst, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if oldest.Messages[0].DocID != undated.DocID || oldest.Messages[len(oldest.Messages)-1].DocID != other.DocID {
		t.Fatalf("oldest order = %#v", oldest.Messages)
	}
}

func TestMailboxManualConversationCompletenessAndDefects(t *testing.T) {
	db, repo := manualScratch(t)
	ctx := context.Background()
	parent := manualMessage(t, db, 11, "ada@example.test", time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC))
	reply := manualMessage(t, db, 12, "owner@example.test", time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC))
	reply.References = []domain.EmailReference{
		{Kind: domain.EmailReferenceInReplyTo, Ordinal: 0, MessageID: parent.MessageID},
		{Kind: domain.EmailReferenceReferences, Ordinal: 0, MessageID: "missing@mail.example.test"},
	}
	// Save out of chronological order: the graph and query must still return
	// the parent first and exactly once.
	if _, err := repo.SaveEmailMessage(ctx, reply); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SaveEmailMessage(ctx, parent); err != nil {
		t.Fatal(err)
	}

	s := manualService(t, db)
	got, err := s.FetchConversation(ctx, ConversationRequest{Ref: encodeRef(refMessage, reply.DocID)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 2 || got.Messages[0].DocID != parent.DocID || got.Messages[1].DocID != reply.DocID {
		t.Fatalf("conversation = %#v", got.Messages)
	}
	if edge := got.Messages[1].Relationship; edge.Method != RelationshipInReplyTo || edge.ParentDocID != parent.DocID {
		t.Errorf("relationship = %#v", edge)
	}
	if len(got.Omissions) == 0 || got.Omissions[0].Reason != OmitMissingAncestor || got.Omissions[0].Count != 1 {
		t.Errorf("omissions = %#v", got.Omissions)
	}

	// A duplicate Message-ID is a defect, not an invitation to choose one
	// document as a parent. It stays in the component but produces no edge.
	duplicate := manualMessage(t, db, 13, "duplicate@example.test", time.Date(2026, 1, 3, 9, 0, 0, 0, time.UTC))
	duplicate.MessageID = parent.MessageID
	if _, err := repo.SaveEmailMessage(ctx, duplicate); err != nil {
		t.Fatal(err)
	}
	again, err := s.FetchConversation(ctx, ConversationRequest{Ref: encodeRef(refConversation, got.ConversationID)})
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Messages) != 3 {
		t.Fatalf("duplicate conversation count = %d", len(again.Messages))
	}
	for _, m := range again.Messages {
		if m.DocID != reply.DocID {
			continue
		}
		if m.Relationship.Method != RelationshipUnresolved {
			t.Errorf("duplicate parent produced an edge: %#v", m.Relationship)
		}
		found := false
		for _, w := range again.Warnings {
			if w.Code == WarnDuplicateIdentifier && w.DocID == reply.DocID {
				found = true
			}
		}
		if !found {
			t.Errorf("duplicate warning missing: %#v", again.Warnings)
		}
	}
}
