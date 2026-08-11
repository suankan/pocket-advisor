package domain

import (
	"reflect"
	"testing"
)

// Identifiers is the join key set that decides a conversation, so its order
// and deduplication are part of the persisted outcome, not a detail.
func TestIdentifiersPutTheOwnIdFirstAndDeduplicate(t *testing.T) {
	m := EmailMessage{
		MessageID: "c@example.test",
		References: []EmailReference{
			{Kind: EmailReferenceInReplyTo, MessageID: "b@example.test"},
			{Kind: EmailReferenceReferences, MessageID: "a@example.test"},
			{Kind: EmailReferenceReferences, MessageID: "b@example.test"},
			{Kind: EmailReferenceReferences, MessageID: "c@example.test"},
		},
	}
	want := []string{"c@example.test", "b@example.test", "a@example.test"}
	if got := m.Identifiers(); !reflect.DeepEqual(got, want) {
		t.Errorf("identifiers = %v, want %v", got, want)
	}
}

// A message with no Message-ID still folds into whatever its reply headers
// name; an absent identifier contributes nothing rather than an empty string.
func TestIdentifiersSkipAbsentValues(t *testing.T) {
	m := EmailMessage{References: []EmailReference{
		{Kind: EmailReferenceInReplyTo, MessageID: "a@example.test"},
		{Kind: EmailReferenceReferences, MessageID: ""},
	}}
	if got := m.Identifiers(); !reflect.DeepEqual(got, []string{"a@example.test"}) {
		t.Errorf("identifiers = %v", got)
	}
	if got := (EmailMessage{}).Identifiers(); len(got) != 0 {
		t.Errorf("identifiers = %v, want none for a header orphan", got)
	}
}

// Conversation identities are derived, not random: the same inputs have to
// reproduce the same conversation on re-ingestion, and different inputs must
// not collide across workspaces.
func TestConversationIdentitiesAreDerivedAndScoped(t *testing.T) {
	if a, b := NewEmailComponentID("ws-1", "a@example.test"), NewEmailComponentID("ws-1", "a@example.test"); a != b {
		t.Errorf("component ids differ across calls: %q, %q", a, b)
	}
	if a, b := NewEmailComponentID("ws-1", "a@example.test"), NewEmailComponentID("ws-2", "a@example.test"); a == b {
		t.Error("one identifier produced the same component in two workspaces")
	}
	if a, b := NewEmailSubjectConversationID("ws-1", "review", "a@example.test"),
		NewEmailSubjectConversationID("ws-2", "review", "a@example.test"); a == b {
		t.Error("one subject produced the same conversation in two workspaces")
	}
	if a, b := NewEmailSubjectConversationID("ws-1", "review", "a@example.test"),
		NewEmailComponentID("ws-1", "review"); a == b {
		t.Error("a subject fallback collided with an identifier component")
	}
	// The participant is what keeps the fallback conservative: a subject as
	// common as "review" must not put two correspondents in one conversation.
	if a, b := NewEmailSubjectConversationID("ws-1", "review", "a@example.test"),
		NewEmailSubjectConversationID("ws-1", "review", "b@example.test"); a == b {
		t.Error("one subject grouped two senders into the same conversation")
	}
	if a, b := NewEmailIsolatedConversationID("doc-1"), NewEmailIsolatedConversationID("doc-2"); a == b {
		t.Error("two isolated messages shared one conversation")
	}
}
