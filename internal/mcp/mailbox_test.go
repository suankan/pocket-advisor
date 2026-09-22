package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
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

func syntheticMailboxTool(t *testing.T, owners []string) *MailboxTool {
	t.Helper()
	base := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	parent := mailbox.Message{DocID: "00000000-0000-4000-8000-000000000001", MessageID: "parent@example.test", Subject: "Synthetic request", SentAt: base, Sender: "sender@example.test", Recipients: []string{"owner@example.test"}}
	followup := mailbox.Message{DocID: "00000000-0000-4000-8000-000000000002", MessageID: "followup@example.test", Subject: "Synthetic follow-up", SentAt: base.Add(time.Hour), Sender: "sender@example.test", Recipients: []string{"owner@example.test"}, References: []domain.EmailReference{{Kind: domain.EmailReferenceInReplyTo, MessageID: parent.MessageID}}}
	store := &syntheticMailboxStore{messages: []mailbox.Message{parent, followup}, now: base.Add(2 * time.Hour)}
	service, err := mailbox.NewWithOwnerIdentities(store, owners, mailbox.DefaultConfig(), nil)
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

func TestMailboxToolAcceptsSenderDomain(t *testing.T) {
	tool := syntheticMailboxTool(t, []string{"owner@example.test"})
	result := mailboxCall(t, tool, tool.ListName(), map[string]any{"sender": "EXAMPLE.TEST", "limit": 1})
	page := result.StructuredContent.(*mailboxResult)
	listed := page.Result.(*mailbox.ListResult)
	if listed.Filters.Sender != "" || listed.Filters.SenderDomain != "example.test" {
		t.Fatalf("applied filters = %#v", listed.Filters)
	}
	validateMailboxResultSchema(t, mailboxListOutputSchema(), page)

	_, err := tool.Call(context.Background(), json.RawMessage(`{"name":"list_messages","arguments":{"sender":"bad_domain"}}`))
	var invalid *argumentError
	if !errors.As(err, &invalid) {
		t.Fatalf("invalid domain error = %T %v", err, err)
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
