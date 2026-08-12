package mailbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/suankan/pocket-advisor/internal/domain"
)

func testService(t *testing.T, store Store) *Service {
	t.Helper()
	s, err := New(store, Config{DefaultLimit: 2, MaxLimit: 3, MaxParticipants: 2}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return s
}

func TestListMessagesFiltersAndOrder(t *testing.T) {
	store := newFakeStore()
	a := synthetic(1, 1, "ada@example.test", at(1, 9))
	b := synthetic(2, 2, "ada@example.test", at(3, 9))
	c := synthetic(3, 3, "bob@example.test", at(2, 9))
	c.Recipients = []string{"friend@example.test", "owner@example.test"}
	undated := synthetic(4, 4, "ada@example.test", zeroTime())
	store.ingest(a)
	store.ingest(b)
	store.ingest(c)
	store.ingest(undated)
	s := testService(t, store)

	res, err := s.ListMessages(context.Background(), ListRequest{
		Sender: "Ada <ADA@EXAMPLE.TEST>", Recipient: "owner@example.test", Limit: 3,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if res.Filters.Sender != "ada@example.test" || res.Filters.Recipient != "owner@example.test" {
		t.Errorf("applied filters = %#v", res.Filters)
	}
	want := []string{b.DocID, a.DocID, undated.DocID}
	if len(res.Messages) != len(want) {
		t.Fatalf("messages = %d, want %d", len(res.Messages), len(want))
	}
	for i, id := range want {
		if res.Messages[i].DocID != id {
			t.Errorf("message %d = %s, want %s", i, res.Messages[i].DocID, id)
		}
	}
	if res.Page.HasMore {
		t.Error("page unexpectedly has more")
	}

	oldest, err := s.ListMessages(context.Background(), ListRequest{Order: OrderOldestFirst, Limit: 3})
	if err != nil {
		t.Fatalf("oldest: %v", err)
	}
	if oldest.Messages[0].DocID != undated.DocID {
		t.Errorf("oldest begins %s, want undated %s", oldest.Messages[0].DocID, undated.DocID)
	}
}

func TestListMessagesSenderDomain(t *testing.T) {
	store := newFakeStore()
	first := synthetic(1, 1, "ada@advisers.example.test", at(1, 9))
	second := synthetic(2, 2, "tanya@advisers.example.test", at(2, 9))
	other := synthetic(3, 3, "other@example.test", at(3, 9))
	store.ingest(first)
	store.ingest(second)
	store.ingest(other)
	s := testService(t, store)

	result, err := s.ListMessages(context.Background(), ListRequest{Sender: "ADVISERS.EXAMPLE.TEST", Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if result.Filters.Sender != "" || result.Filters.SenderDomain != "advisers.example.test" {
		t.Fatalf("applied filters = %#v", result.Filters)
	}
	if len(result.Messages) != 2 || result.Messages[0].DocID != second.DocID || result.Messages[1].DocID != first.DocID {
		t.Fatalf("messages = %#v", result.Messages)
	}

	for _, invalid := range []string{"advisers.example.test.", "advisers_example.test", "@advisers.example.test"} {
		if _, err := s.ListMessages(context.Background(), ListRequest{Sender: invalid}); err == nil {
			t.Errorf("sender %q was accepted", invalid)
		}
	}
}

func TestListMessagesDateBoundsExcludeUndatedWithWarning(t *testing.T) {
	store := newFakeStore()
	inside := synthetic(1, 1, "ada@example.test", at(2, 9))
	before := synthetic(2, 2, "ada@example.test", at(1, 9))
	undated := synthetic(3, 3, "ada@example.test", zeroTime())
	store.ingest(inside)
	store.ingest(before)
	store.ingest(undated)

	res, err := testService(t, store).ListMessages(context.Background(), ListRequest{After: at(2, 0), Before: at(3, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 1 || res.Messages[0].DocID != inside.DocID {
		t.Errorf("messages = %#v", res.Messages)
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Code != WarnUndatedExcluded {
		t.Errorf("warnings = %#v", res.Warnings)
	}
}

func TestListMessagesPaginationHasAStableSnapshot(t *testing.T) {
	store := newFakeStore()
	first := synthetic(1, 1, "ada@example.test", at(4, 9))
	second := synthetic(2, 2, "ada@example.test", at(3, 9))
	third := synthetic(3, 3, "ada@example.test", at(2, 9))
	store.ingest(first)
	store.ingest(second)
	store.ingest(third)
	s := testService(t, store)

	page1, err := s.ListMessages(context.Background(), ListRequest{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !page1.Page.HasMore || page1.Page.NextCursor == "" {
		t.Fatalf("page = %#v", page1.Page)
	}
	// A backfill belongs before the cursor by date but arrives after the
	// snapshot. Without ingested_at <= watermark it would turn page 2 into a
	// duplicate/skipping set.
	backfill := synthetic(4, 4, "ada@example.test", at(3, 12))
	store.ingest(backfill)
	page2, err := s.ListMessages(context.Background(), ListRequest{Limit: 2, Cursor: page1.Page.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(page2.Messages) != 1 || page2.Messages[0].DocID != third.DocID {
		t.Errorf("second page = %#v, want only %s", page2.Messages, third.DocID)
	}

	fresh, err := s.ListMessages(context.Background(), ListRequest{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh.Messages) != 3 || fresh.Messages[1].DocID != backfill.DocID {
		t.Errorf("fresh page does not include backfill in order: %#v", fresh.Messages)
	}
}

func TestListMessagesRejectsCursorChanges(t *testing.T) {
	store := newFakeStore()
	store.ingest(synthetic(1, 1, "ada@example.test", at(3, 9)))
	store.ingest(synthetic(2, 2, "ada@example.test", at(2, 9)))
	store.ingest(synthetic(3, 3, "ada@example.test", at(1, 9)))
	s := testService(t, store)
	first, err := s.ListMessages(context.Background(), ListRequest{Limit: 1, Sender: "ada@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ListMessages(context.Background(), ListRequest{Cursor: first.Page.NextCursor, Sender: "bob@example.test"})
	if !errors.Is(err, ErrCursorFilters) {
		t.Fatalf("err = %v", err)
	}
	_, err = s.ListMessages(context.Background(), ListRequest{Cursor: first.Page.NextCursor, Sender: "ada@example.test", Order: OrderOldestFirst})
	if !errors.Is(err, ErrCursorOrder) {
		t.Fatalf("err = %v", err)
	}
}

func TestListMessagesCollapsesConversations(t *testing.T) {
	store := newFakeStore()
	old := synthetic(1, 1, "ada@example.test", at(1, 9))
	latest := synthetic(2, 1, "bob@example.test", at(3, 9))
	other := synthetic(3, 2, "carol@example.test", at(2, 9))
	store.ingest(old)
	store.ingest(latest)
	store.ingest(other)

	res, err := testService(t, store).ListMessages(context.Background(), ListRequest{CollapseConversations: true, Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 2 || res.Messages[0].DocID != latest.DocID {
		t.Fatalf("messages = %#v", res.Messages)
	}
	summary := res.Messages[0].Conversation
	if summary == nil || summary.MessageCount != 2 || summary.MatchedCount != 2 {
		t.Errorf("summary = %#v", summary)
	}
	if len(res.Omissions) == 0 || res.Omissions[0] != (Omission{Reason: OmitCollapsedMessages, Count: 1}) {
		t.Errorf("omissions = %#v", res.Omissions)
	}
}

func TestFetchConversationIsChronologicalAndComplete(t *testing.T) {
	store := newFakeStore()
	parent := synthetic(1, 1, "ada@example.test", at(2, 9))
	parent.MessageID = "parent@mail.example.test"
	reply := synthetic(2, 1, "owner@example.test", at(3, 9))
	reply.MessageID = "reply@mail.example.test"
	reply.References = []domain.EmailReference{{Kind: domain.EmailReferenceInReplyTo, MessageID: parent.MessageID}}
	undated := synthetic(3, 1, "bob@example.test", zeroTime())
	store.ingest(reply) // deliberately out of chronological ingestion order
	store.ingest(undated)
	store.ingest(parent)

	s := testService(t, store)
	list, err := s.ListMessages(context.Background(), ListRequest{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.FetchConversation(context.Background(), ConversationRequest{Ref: list.Messages[0].Ref})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("message count = %d", len(got.Messages))
	}
	want := []string{parent.DocID, reply.DocID, undated.DocID}
	for i, id := range want {
		if got.Messages[i].DocID != id {
			t.Errorf("message %d = %s, want %s", i, got.Messages[i].DocID, id)
		}
	}
	if edge := got.Messages[1].Relationship; edge.Method != RelationshipInReplyTo || edge.ParentDocID != parent.DocID {
		t.Errorf("reply relationship = %#v", edge)
	}
}

func TestFetchConversationHidesReferenceExistence(t *testing.T) {
	store := newFakeStore()
	s := testService(t, store)
	_, err := s.FetchConversation(context.Background(), ConversationRequest{Ref: encodeRef(refMessage, docID(3))})
	if !errors.Is(err, ErrUnknownReference) {
		t.Fatalf("err = %v", err)
	}
}

func zeroTime() time.Time { return time.Time{} }
