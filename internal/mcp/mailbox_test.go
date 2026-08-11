package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/mailbox"
)

type syntheticMailboxStore struct {
	messages []mailbox.Message
	now      time.Time
}

func (s *syntheticMailboxStore) Snapshot(context.Context) (time.Time, error) { return s.now, nil }
func (s *syntheticMailboxStore) ListMessages(_ context.Context, q mailbox.PageQuery) ([]mailbox.Message, error) {
	out := append([]mailbox.Message(nil), s.messages...)
	sort.Slice(out, func(i, j int) bool { return out[i].SentAt.After(out[j].SentAt) })
	if q.Order == mailbox.OrderOldestFirst {
		sort.Slice(out, func(i, j int) bool { return out[i].SentAt.Before(out[j].SentAt) })
	}
	if len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}
func (s *syntheticMailboxStore) Summaries(context.Context, string, []string, time.Time) (map[string]mailbox.Aggregate, error) {
	return map[string]mailbox.Aggregate{}, nil
}
func (s *syntheticMailboxStore) CandidateMessages(context.Context, mailbox.CandidateQuery) ([]mailbox.Message, error) {
	return append([]mailbox.Message(nil), s.messages...), nil
}
func (s *syntheticMailboxStore) ConversationOf(_ context.Context, _ string, docID string) (string, error) {
	for _, m := range s.messages {
		if m.DocID == docID {
			return m.ConversationID, nil
		}
	}
	return "", mailbox.ErrUnknownReference
}
func (s *syntheticMailboxStore) ConversationMessages(_ context.Context, _ string, conversationID string, _ time.Time) ([]mailbox.Message, error) {
	var out []mailbox.Message
	for _, m := range s.messages {
		if m.ConversationID == conversationID {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return nil, mailbox.ErrUnknownReference
	}
	return out, nil
}

func syntheticMailboxTool(t *testing.T, owners []string) *MailboxTool {
	t.Helper()
	base := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	conversation := "11111111-0000-4000-8000-000000000001"
	parent := mailbox.Message{DocID: "00000000-0000-4000-8000-000000000001", MessageID: "parent@example.test", ConversationID: conversation, ConversationMethod: domain.ConversationByReferences, Subject: "Synthetic request", SentAt: base, Sender: "sender@example.test", Recipients: []string{"owner@example.test"}}
	followup := mailbox.Message{DocID: "00000000-0000-4000-8000-000000000002", MessageID: "followup@example.test", ConversationID: conversation, ConversationMethod: domain.ConversationByReferences, Subject: "Synthetic follow-up", SentAt: base.Add(time.Hour), Sender: "sender@example.test", Recipients: []string{"owner@example.test"}, References: []domain.EmailReference{{Kind: domain.EmailReferenceInReplyTo, MessageID: parent.MessageID}}}
	store := &syntheticMailboxStore{messages: []mailbox.Message{parent, followup}, now: base.Add(2 * time.Hour)}
	service, err := mailbox.NewWithOwnerIdentities(store, "synthetic", owners, mailbox.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return &MailboxTool{Service: service, Workspace: "synthetic", Title: "Synthetic mailbox"}
}

type mailboxCaller interface {
	Call(context.Context, json.RawMessage) (CallToolResult, error)
}

func mailboxCall(t *testing.T, tool mailboxCaller, name string, arguments any) CallToolResult {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"name": name, "arguments": arguments})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.Call(context.Background(), raw)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	return result
}

func TestMailboxToolsExposeTypedFixedWorkspaceResults(t *testing.T) {
	tool := syntheticMailboxTool(t, []string{"owner@example.test"})
	wrapped := &QueryTool{Workspace: "synthetic", Mailbox: tool}
	definitions := wrapped.DescribeAll()
	if len(definitions) != 5 || definitions[2].Name != "list_messages_synthetic" || definitions[3].Name != "fetch_conversation_synthetic" || definitions[4].Name != "awaiting_reply_candidates_synthetic" {
		t.Fatalf("definitions = %+v", definitions)
	}

	list := mailboxCall(t, wrapped, tool.ListName(), map[string]any{"sender": "sender@example.test", "limit": 1})
	page, ok := list.StructuredContent.(*mailboxResult)
	if !ok || page.Kind != "message_list" {
		t.Fatalf("list content = %#v", list.StructuredContent)
	}
	listed := page.Result.(*mailbox.ListResult)
	if len(listed.Messages) != 1 || listed.Messages[0].Ref == "" || listed.Messages[0].ConversationRef == "" || listed.Page.Snapshot.IsZero() {
		t.Fatalf("list result = %+v", listed)
	}
	validateMailboxResultSchema(t, mailboxListOutputSchema(), page)
	assertMailboxBound(t, list)

	conversation := mailboxCall(t, tool, tool.ConversationName(), map[string]any{"ref": listed.Messages[0].Ref})
	conversationPage := conversation.StructuredContent.(*mailboxResult)
	fetched := conversationPage.Result.(*mailbox.ConversationResult)
	if conversationPage.Kind != "conversation" || len(fetched.Messages) != 2 || fetched.Messages[0].SentAt.After(*fetched.Messages[1].SentAt) {
		t.Fatalf("conversation result = %+v", fetched)
	}
	validateMailboxResultSchema(t, mailboxConversationOutputSchema(), conversationPage)
	assertMailboxBound(t, conversation)

	candidates := mailboxCall(t, tool, tool.AwaitingReplyName(), map[string]any{"participant": "sender@example.test"})
	candidatePage := candidates.StructuredContent.(*mailboxResult)
	awaiting := candidatePage.Result.(*mailbox.AwaitingReplyResult)
	if candidatePage.Kind != "awaiting_reply_candidates" || len(awaiting.Candidates) != 1 || awaiting.Candidates[0].LatestInbound.Ref == "" {
		t.Fatalf("candidate result = %+v", awaiting)
	}
	validateMailboxResultSchema(t, mailboxAwaitingReplyOutputSchema(), candidatePage)
	assertMailboxBound(t, candidates)
}

func TestMailboxToolRejectsScopeAndRequiresOwners(t *testing.T) {
	tool := syntheticMailboxTool(t, nil)
	schema := compileJSONSchema(t, "mailbox-list.schema.json", mailboxListInputSchema())
	if err := schema.Validate(map[string]any{"workspace": "other"}); err == nil {
		t.Fatal("workspace selector passed closed schema")
	}
	raw := json.RawMessage(`{"name":"list_messages_synthetic","arguments":{"workspace":"other"}}`)
	_, err := tool.Call(context.Background(), raw)
	var invalid *argumentError
	if !errors.As(err, &invalid) {
		t.Fatalf("scope error = %T %v", err, err)
	}

	_, err = tool.Call(context.Background(), json.RawMessage(`{"name":"awaiting_reply_candidates_synthetic","arguments":{}}`))
	if !errors.As(err, &invalid) || !strings.Contains(err.Error(), "owner identities") {
		t.Fatalf("owner configuration error = %T %v", err, err)
	}
}

func validateMailboxResultSchema(t *testing.T, document map[string]any, page *mailboxResult) {
	t.Helper()
	schema := compileJSONSchema(t, "mailbox-result.schema.json", document)
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(value); err != nil {
		t.Fatalf("result does not match schema: %v", err)
	}
}

func assertMailboxBound(t *testing.T, result CallToolResult) {
	t.Helper()
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > targetToolResultBytes {
		t.Fatalf("result = %d bytes", len(encoded))
	}
	if lines := readableLines(result.Content[0].Text); lines > targetReadableLines {
		t.Fatalf("result = %d lines", lines)
	}
}
