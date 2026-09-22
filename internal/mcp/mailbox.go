package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/suankan/pocket-advisor/internal/mailbox"
)

const (
	maxMailboxAddressRunes = 320
	maxMailboxLimit        = 200
	maxCandidateLimit      = 100
	maxMailboxDateRunes    = 64
)

// MailboxTool exposes exact, fixed-workspace email browsing alongside
// retrieval. The service owns the workspace scope; tool arguments are only
// closed mailbox filters and server-issued references.
type MailboxTool struct {
	Service   *mailbox.Service
	Workspace string
	Title     string
	// Log traces every list_messages/fetch_conversation/awaiting_reply_candidates
	// call: the filters applied and what came back. Nil-safe, see QueryTool.Log.
	Log *slog.Logger
}

func (t *MailboxTool) logger() *slog.Logger {
	if t.Log != nil {
		return t.Log
	}
	return slog.Default()
}

func (t *MailboxTool) normalizedWorkspace() string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		default:
			return '_'
		}
	}, t.Workspace)
}

func (t *MailboxTool) ListName() string { return "list_messages" }

func (t *MailboxTool) DescribeAll() []ToolDefinition {
	title := t.Title
	if title == "" {
		title = t.Workspace
	}
	return []ToolDefinition{
		{Name: t.ListName(), Title: "List " + title + " email messages", Description: "List exact email messages in this fixed workspace. Filter sender by one exact normalized mailbox or one exact normalized domain; filter recipient only by an exact normalized mailbox. Also filter by date, direction, and order. Results contain server-issued message and conversation references; pass a returned pagination cursor unchanged. This is deterministic mailbox evidence, not generated advice. Do not provide a workspace, SQL, or a constructed reference. Each message reports where to find it relative to the workspace local staging directory on the operator machine: source_path is the message own staged file, and when that is absent container_source_path names the file it was extracted from. Report the path given; never infer one from where similar messages live.", InputSchema: mailboxListInputSchema(), OutputSchema: mailboxListOutputSchema(), Annotations: readOnlyAnnotations()},
	}
}

type mailboxListArguments struct {
	Sender    string            `json:"sender,omitempty"`
	Recipient string            `json:"recipient,omitempty"`
	After     *string           `json:"after,omitempty"`
	Before    *string           `json:"before,omitempty"`
	Order     mailbox.Order     `json:"order,omitempty"`
	Limit     int               `json:"limit,omitempty"`
	Cursor    *string           `json:"cursor,omitempty"`
	Direction mailbox.Direction `json:"direction,omitempty"`
}

// Call validates and dispatches the mailbox-only tool set. QueryTool calls it
// before its retrieval dispatcher, so unknown mailbox names remain MCP unknown
// tools rather than becoming a storage operation.
func (t *MailboxTool) Call(ctx context.Context, raw json.RawMessage) (CallToolResult, error) {
	if t == nil || t.Service == nil || strings.TrimSpace(t.Workspace) == "" {
		return CallToolResult{}, fmt.Errorf("mailbox service is unavailable")
	}
	var params rawCallParams
	if err := decodeStrict(raw, &params); err != nil {
		return CallToolResult{}, &argumentError{message: "tools/call params must be a valid object"}
	}
	if params.Name == "" {
		return CallToolResult{}, &argumentError{message: "tool name is required"}
	}
	if len(params.Task) > 0 && string(params.Task) != "null" {
		return CallToolResult{}, &argumentError{message: "task-augmented execution is not supported"}
	}
	switch params.Name {
	case t.ListName():
		var args mailboxListArguments
		if err := decodeStrict(params.Arguments, &args); err != nil {
			return CallToolResult{}, &argumentError{message: "arguments must match the advertised mailbox-list input schema"}
		}
		if err := validateMailboxAddress(args.Sender, "sender"); err != nil {
			return CallToolResult{}, err
		}
		if err := validateMailboxAddress(args.Recipient, "recipient"); err != nil {
			return CallToolResult{}, err
		}
		if args.Limit > maxMailboxLimit {
			return CallToolResult{}, &argumentError{message: "limit exceeds the mailbox page bound"}
		}
		if args.Cursor != nil && (len(*args.Cursor) == 0 || len(*args.Cursor) > maxCursorBytes) {
			return CallToolResult{}, &argumentError{message: "cursor must be a bounded opaque token returned by this tool"}
		}
		var cursor string
		if args.Cursor != nil {
			cursor = *args.Cursor
		}
		after, err := parseMailboxDate(args.After)
		if err != nil {
			return CallToolResult{}, err
		}
		before, err := parseMailboxDate(args.Before)
		if err != nil {
			return CallToolResult{}, err
		}
		result, err := t.Service.ListMessages(ctx, mailbox.ListRequest{Sender: args.Sender, Recipient: args.Recipient, After: after, Before: before, Order: args.Order, Limit: args.Limit, Cursor: cursor, Direction: args.Direction})
		if err != nil {
			t.logger().Info("list_messages", "sender", args.Sender, "recipient", args.Recipient,
				"direction", args.Direction, "limit", args.Limit, "cursor_set", cursor != "", "error", err.Error())
			return CallToolResult{}, mailboxArgumentError(err)
		}
		t.logger().Info("list_messages", "sender", args.Sender, "recipient", args.Recipient,
			"direction", args.Direction, "limit", args.Limit, "cursor_set", cursor != "",
			"returned", len(result.Messages), "has_more", result.Page.HasMore)
		return finalizeMailboxResult("message_list", result)
	default:
		return CallToolResult{}, &unknownToolError{}
	}
}

type mailboxResult struct {
	Kind           string         `json:"kind"`
	Result         any            `json:"result"`
	ResponseBudget ResponseBudget `json:"response_budget"`
}

func finalizeMailboxResult(kind string, value any) (CallToolResult, error) {
	page := &mailboxResult{Kind: kind, Result: value, ResponseBudget: ResponseBudget{Allowed: targetToolResultBytes, Unit: utf8ByteUnit}}
	var result CallToolResult
	for range 4 {
		text := renderMailboxResult(page)
		result = CallToolResult{Content: []TextContent{{Type: "text", Text: text}}, StructuredContent: page, IsError: false}
		encoded, err := json.Marshal(result)
		if err != nil {
			return CallToolResult{}, fmt.Errorf("encode mailbox result: %w", err)
		}
		if page.ResponseBudget.Used == len(encoded) {
			break
		}
		page.ResponseBudget.Used = len(encoded)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return CallToolResult{}, fmt.Errorf("encode mailbox result: %w", err)
	}
	if len(encoded) > targetToolResultBytes {
		return CallToolResult{}, &resultSizeError{limit: targetToolResultBytes}
	}
	if readableLines(result.Content[0].Text) > targetReadableLines {
		return CallToolResult{}, &resultLineError{limit: targetReadableLines}
	}
	return result, nil
}

// formatSentAt renders the exact date sent_at always carries in
// structuredContent into the human-readable text block too — clients
// commonly summarize from the prose rather than parse structured content, so
// a date present only there tends to be lost or vaguely paraphrased.
func formatSentAt(t *time.Time) string {
	if t == nil {
		return "undated"
	}
	return t.UTC().Format(time.RFC3339)
}

func renderMailboxResult(page *mailboxResult) string {
	switch result := page.Result.(type) {
	case *mailbox.ListResult:
		var b strings.Builder
		fmt.Fprintf(&b, "Mailbox message list: %d message(s), %s order.\n", len(result.Messages), result.Order)
		for _, m := range result.Messages {
			fmt.Fprintf(&b, "[%s] %s — %s — %s\n", m.DocID, formatSentAt(m.SentAt), m.Subject, m.Sender)
		}
		if result.Page.HasMore {
			fmt.Fprintf(&b, "More messages are available. Call this tool again with exactly {\"cursor\":%q} and the same filters.\n", result.Page.NextCursor)
		}
		fmt.Fprintf(&b, "Snapshot: %s. Response budget: %d of %d UTF-8 bytes.\n", result.Page.Snapshot.Format(time.RFC3339), page.ResponseBudget.Used, page.ResponseBudget.Allowed)
		return b.String()
	default:
		return ""
	}
}

func mailboxListInputSchema() map[string]any { return mailboxFilterSchema(true) }
func mailboxFilterSchema(includeCursor bool) map[string]any {
	props := map[string]any{"sender": map[string]any{"type": "string", "maxLength": maxMailboxAddressRunes, "description": "One exact normalized sender mailbox or one exact normalized sender domain."}, "recipient": map[string]any{"type": "string", "maxLength": maxMailboxAddressRunes, "description": "One exact normalized recipient mailbox."}, "after": nullableDateSchema(), "before": nullableDateSchema(), "order": map[string]any{"enum": []string{"newest_first", "oldest_first"}}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": maxMailboxLimit}, "direction": map[string]any{"enum": []string{"either", "inbound", "outbound"}}}
	if includeCursor {
		props["cursor"] = map[string]any{"type": "string", "minLength": 1, "maxLength": maxCursorBytes}
	}
	return map[string]any{"$schema": jsonSchema202012, "type": "object", "additionalProperties": false, "properties": props}
}
func nullableDateSchema() map[string]any {
	return map[string]any{"type": []string{"string", "null"}, "maxLength": maxMailboxDateRunes}
}

func mailboxResultOutputSchema(kind, resultRef string) map[string]any {
	return map[string]any{
		"$schema": jsonSchema202012, "type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"kind": map[string]any{"const": kind}, "result": map[string]any{"$ref": resultRef}, "response_budget": map[string]any{"$ref": "#/$defs/budget"},
		},
		"required": []string{"kind", "result", "response_budget"}, "$defs": mailboxOutputDefs(),
	}
}

func mailboxListOutputSchema() map[string]any {
	return mailboxResultOutputSchema("message_list", "#/$defs/list_result")
}

func mailboxMessageSchema(extra map[string]any, nullableTime, stringArray map[string]any) map[string]any {
	properties := map[string]any{
		"doc_id": map[string]any{"type": "string", "minLength": 1}, "subject": map[string]any{"type": "string"}, "sent_at": nullableTime, "sender": map[string]any{"type": "string"}, "recipients": stringArray, "automated_class": map[string]any{"type": "string"}, "list_id": map[string]any{"type": "string"}, "source_locations": map[string]any{"type": "array", "items": map[string]any{"type": "object"}}, "source_path": map[string]any{"type": "string"}, "container_source_path": map[string]any{"type": "string"},
	}
	required := []string{"doc_id", "subject", "sent_at", "sender", "recipients"}
	for name, schema := range extra {
		properties[name] = schema
		required = append(required, name)
	}
	return map[string]any{"type": "object", "additionalProperties": false, "properties": properties, "required": required}
}

func mailboxOutputDefs() map[string]any {
	stringArray := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	nullableTime := map[string]any{"oneOf": []any{map[string]any{"type": "string", "format": "date-time"}, map[string]any{"type": "null"}}}
	warning := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"code": map[string]any{"type": "string"}, "doc_id": map[string]any{"type": "string"}}, "required": []string{"code"}}
	omission := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"reason": map[string]any{"type": "string"}, "count": map[string]any{"type": "integer", "minimum": 1}}, "required": []string{"reason", "count"}}
	message := mailboxMessageSchema(nil, nullableTime, stringArray)
	return map[string]any{
		"budget":  map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"used": map[string]any{"type": "integer", "minimum": 1}, "allowed": map[string]any{"const": targetToolResultBytes}, "unit": map[string]any{"const": utf8ByteUnit}}, "required": []string{"used", "allowed", "unit"}},
		"warning": warning, "omission": omission, "message": message,
		"list_result": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
			"messages": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/message"}}, "filters": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"sender": map[string]any{"type": "string"}, "sender_domain": map[string]any{"type": "string"}, "recipient": map[string]any{"type": "string"}, "after": nullableTime, "before": nullableTime, "direction": map[string]any{"type": "string"}}}, "order": map[string]any{"enum": []string{"newest_first", "oldest_first"}}, "page": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"limit": map[string]any{"type": "integer", "minimum": 1}, "returned": map[string]any{"type": "integer", "minimum": 0}, "has_more": map[string]any{"type": "boolean"}, "next_cursor": map[string]any{"type": "string"}, "snapshot": map[string]any{"type": "string", "format": "date-time"}}, "required": []string{"limit", "returned", "has_more", "snapshot"}}, "omissions": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/omission"}}, "warnings": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/warning"}},
		}, "required": []string{"messages", "filters", "order", "page", "omissions", "warnings"}},
	}
}

func parseMailboxDate(value *string) (time.Time, error) {
	if value == nil {
		return time.Time{}, nil
	}
	if len(*value) > maxMailboxDateRunes {
		return time.Time{}, &argumentError{message: "date must be a bounded RFC 3339 timestamp or YYYY-MM-DD date"}
	}
	trimmed := strings.TrimSpace(*value)
	if parsed, err := time.Parse(time.RFC3339, trimmed); err == nil {
		return parsed.UTC(), nil
	}
	if parsed, err := time.Parse("2006-01-02", trimmed); err == nil {
		return parsed.UTC(), nil
	}
	return time.Time{}, &argumentError{message: "date must be an RFC 3339 timestamp or YYYY-MM-DD date"}
}

func validateMailboxAddress(value, name string) error {
	if utf8.RuneCountInString(value) > maxMailboxAddressRunes {
		return &argumentError{message: name + " exceeds the mailbox address bound"}
	}
	return nil
}

func mailboxArgumentError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, mailbox.ErrOwnerIdentitiesRequired) {
		return &argumentError{message: "direction-dependent mailbox operations require configured owner identities"}
	}
	if errors.Is(err, mailbox.ErrUnknownReference) {
		return &argumentError{message: "reference is invalid or unavailable in this workspace"}
	}
	if errors.Is(err, mailbox.ErrCursorMalformed) || errors.Is(err, mailbox.ErrCursorVersion) || errors.Is(err, mailbox.ErrCursorOrder) || errors.Is(err, mailbox.ErrCursorFilters) {
		return &argumentError{message: "pagination cursor is invalid for this mailbox request; start a new listing"}
	}
	// Validation errors from mailbox intentionally do not echo header values or
	// addresses. Treat them as correctable arguments; dependency failures retain
	// the bounded generic MCP error path.
	if strings.Contains(err.Error(), "filter") || strings.Contains(err.Error(), "date range") || strings.Contains(err.Error(), "sort order") || strings.Contains(err.Error(), "direction") {
		return &argumentError{message: "mailbox filters are invalid; correct the request and retry"}
	}
	return err
}
