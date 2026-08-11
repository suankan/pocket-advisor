package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func (t *MailboxTool) ListName() string          { return "list_messages" }
func (t *MailboxTool) ConversationName() string  { return "fetch_conversation" }
func (t *MailboxTool) AwaitingReplyName() string { return "awaiting_reply_candidates" }

func (t *MailboxTool) DescribeAll() []ToolDefinition {
	title := t.Title
	if title == "" {
		title = t.Workspace
	}
	return []ToolDefinition{
		{Name: t.ListName(), Title: "List " + title + " email messages", Description: "List exact email messages in this fixed workspace. Filter sender by one exact normalized mailbox or one exact normalized domain; filter recipient only by an exact normalized mailbox. Also filter by date, direction, and order. Results contain server-issued message and conversation references; pass a returned pagination cursor unchanged. This is deterministic mailbox evidence, not generated advice. Do not provide a workspace, SQL, or a constructed reference.", InputSchema: mailboxListInputSchema(), OutputSchema: mailboxListOutputSchema(), Annotations: readOnlyAnnotations()},
		{Name: t.ConversationName(), Title: "Fetch " + title + " conversation", Description: "Fetch every email message in one fixed-workspace conversation in stable chronological order. Pass exactly a server-issued message or conversation reference returned by mailbox browsing. Relationship method, warnings, omissions, and evidence references are included. Do not construct a reference or provide a workspace.", InputSchema: mailboxConversationInputSchema(), OutputSchema: mailboxConversationOutputSchema(), Annotations: readOnlyAnnotations()},
		{Name: t.AwaitingReplyName(), Title: "Find " + title + " awaiting-reply candidates", Description: "List bounded deterministic awaiting-reply candidates for this fixed workspace. A candidate is not a conclusion that action is required: inspect its latest inbound message, later events, participants, relationship method, and warnings. Optional participant and date filters select the inbound candidate only. Do not provide owner identities, a workspace, SQL, or a constructed reference.", InputSchema: mailboxAwaitingReplyInputSchema(), OutputSchema: mailboxAwaitingReplyOutputSchema(), Annotations: readOnlyAnnotations()},
	}
}

type mailboxListArguments struct {
	Sender    string            `json:"sender,omitempty"`
	Recipient string            `json:"recipient,omitempty"`
	After     *string           `json:"after,omitempty"`
	Before    *string           `json:"before,omitempty"`
	Order     mailbox.Order     `json:"order,omitempty"`
	Limit     int               `json:"limit,omitempty"`
	Collapse  bool              `json:"collapse_conversations,omitempty"`
	Cursor    *string           `json:"cursor,omitempty"`
	Direction mailbox.Direction `json:"direction,omitempty"`
}

type mailboxConversationArguments struct {
	Ref string `json:"ref"`
}
type mailboxAwaitingReplyArguments struct {
	Participant string  `json:"participant,omitempty"`
	After       *string `json:"after,omitempty"`
	Before      *string `json:"before,omitempty"`
	Limit       int     `json:"limit,omitempty"`
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
		result, err := t.Service.ListMessages(ctx, mailbox.ListRequest{Sender: args.Sender, Recipient: args.Recipient, After: after, Before: before, Order: args.Order, Limit: args.Limit, CollapseConversations: args.Collapse, Cursor: cursor, Direction: args.Direction})
		if err != nil {
			return CallToolResult{}, mailboxArgumentError(err)
		}
		return finalizeMailboxResult("message_list", result)
	case t.ConversationName():
		var args mailboxConversationArguments
		if err := decodeStrict(params.Arguments, &args); err != nil {
			return CallToolResult{}, &argumentError{message: "arguments must match the advertised conversation input schema"}
		}
		if args.Ref == "" || len(args.Ref) > maxCursorBytes {
			return CallToolResult{}, &argumentError{message: "ref must be a bounded server-issued message or conversation reference"}
		}
		result, err := t.Service.FetchConversation(ctx, mailbox.ConversationRequest{Ref: args.Ref})
		if err != nil {
			return CallToolResult{}, mailboxArgumentError(err)
		}
		return finalizeMailboxResult("conversation", result)
	case t.AwaitingReplyName():
		var args mailboxAwaitingReplyArguments
		if err := decodeStrict(params.Arguments, &args); err != nil {
			return CallToolResult{}, &argumentError{message: "arguments must match the advertised awaiting-reply input schema"}
		}
		if err := validateMailboxAddress(args.Participant, "participant"); err != nil {
			return CallToolResult{}, err
		}
		if args.Limit > maxCandidateLimit {
			return CallToolResult{}, &argumentError{message: "limit exceeds the awaiting-reply page bound"}
		}
		after, err := parseMailboxDate(args.After)
		if err != nil {
			return CallToolResult{}, err
		}
		before, err := parseMailboxDate(args.Before)
		if err != nil {
			return CallToolResult{}, err
		}
		result, err := t.Service.AwaitingReplyCandidates(ctx, mailbox.AwaitingReplyRequest{Participant: args.Participant, After: after, Before: before, Limit: args.Limit})
		if err != nil {
			return CallToolResult{}, mailboxArgumentError(err)
		}
		return finalizeMailboxResult("awaiting_reply_candidates", result)
	default:
		return CallToolResult{}, &unknownToolError{}
	}
}

func validateMailboxAddress(value, name string) error {
	if utf8.RuneCountInString(value) > maxMailboxAddressRunes {
		return &argumentError{message: name + " exceeds the mailbox address bound"}
	}
	return nil
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

func renderMailboxResult(page *mailboxResult) string {
	switch result := page.Result.(type) {
	case *mailbox.ListResult:
		var b strings.Builder
		fmt.Fprintf(&b, "Mailbox message list: %d message(s), %s order.\n", len(result.Messages), result.Order)
		for _, m := range result.Messages {
			fmt.Fprintf(&b, "[%s] %s — %s\n", m.Ref, m.Subject, m.Sender)
		}
		if result.Page.HasMore {
			fmt.Fprintf(&b, "More messages are available. Call this tool again with exactly {\"cursor\":%q} and the same filters.\n", result.Page.NextCursor)
		}
		fmt.Fprintf(&b, "Snapshot: %s. Response budget: %d of %d UTF-8 bytes.\n", result.Page.Snapshot.Format(time.RFC3339), page.ResponseBudget.Used, page.ResponseBudget.Allowed)
		return b.String()
	case *mailbox.ConversationResult:
		var b strings.Builder
		fmt.Fprintf(&b, "Conversation [%s]: %d message(s) in chronological order.\n", result.ConversationRef, len(result.Messages))
		for _, m := range result.Messages {
			fmt.Fprintf(&b, "[%s] %s — %s (%s)\n", m.Ref, m.Subject, m.Sender, m.Relationship.Method)
		}
		fmt.Fprintf(&b, "Snapshot: %s. Response budget: %d of %d UTF-8 bytes.\n", result.Snapshot.Format(time.RFC3339), page.ResponseBudget.Used, page.ResponseBudget.Allowed)
		return b.String()
	case *mailbox.AwaitingReplyResult:
		var b strings.Builder
		fmt.Fprintf(&b, "Awaiting-reply candidates: %d. These are candidates, not action conclusions.\n", len(result.Candidates))
		for _, c := range result.Candidates {
			fmt.Fprintf(&b, "[%s] latest inbound [%s] %s\n", c.ConversationRef, c.LatestInbound.Ref, c.LatestInbound.Subject)
		}
		fmt.Fprintf(&b, "Snapshot: %s. Response budget: %d of %d UTF-8 bytes.\n", result.Snapshot.Format(time.RFC3339), page.ResponseBudget.Used, page.ResponseBudget.Allowed)
		return b.String()
	default:
		return "Mailbox result."
	}
}

func mailboxListInputSchema() map[string]any { return mailboxFilterSchema(true) }
func mailboxAwaitingReplyInputSchema() map[string]any {
	return map[string]any{"$schema": jsonSchema202012, "type": "object", "additionalProperties": false, "properties": map[string]any{
		"participant": map[string]any{"type": "string", "maxLength": maxMailboxAddressRunes}, "after": nullableDateSchema(), "before": nullableDateSchema(), "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": maxCandidateLimit},
	}}
}
func mailboxConversationInputSchema() map[string]any {
	return map[string]any{"$schema": jsonSchema202012, "type": "object", "additionalProperties": false, "properties": map[string]any{"ref": map[string]any{"type": "string", "minLength": 1, "maxLength": maxCursorBytes}}, "required": []string{"ref"}}
}
func mailboxFilterSchema(includeCursor bool) map[string]any {
	props := map[string]any{"sender": map[string]any{"type": "string", "maxLength": maxMailboxAddressRunes, "description": "One exact normalized sender mailbox or one exact normalized sender domain."}, "recipient": map[string]any{"type": "string", "maxLength": maxMailboxAddressRunes, "description": "One exact normalized recipient mailbox."}, "after": nullableDateSchema(), "before": nullableDateSchema(), "order": map[string]any{"enum": []string{"newest_first", "oldest_first"}}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": maxMailboxLimit}, "collapse_conversations": map[string]any{"type": "boolean"}, "direction": map[string]any{"enum": []string{"either", "inbound", "outbound"}}}
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
func mailboxConversationOutputSchema() map[string]any {
	return mailboxResultOutputSchema("conversation", "#/$defs/conversation_result")
}
func mailboxAwaitingReplyOutputSchema() map[string]any {
	return mailboxResultOutputSchema("awaiting_reply_candidates", "#/$defs/awaiting_reply_result")
}

func mailboxMessageSchema(extra map[string]any, nullableTime, stringArray map[string]any) map[string]any {
	properties := map[string]any{
		"ref": map[string]any{"type": "string", "minLength": 1}, "doc_id": map[string]any{"type": "string", "minLength": 1}, "conversation_ref": map[string]any{"type": "string", "minLength": 1}, "conversation_id": map[string]any{"type": "string", "minLength": 1}, "conversation_method": map[string]any{"type": "string", "minLength": 1}, "subject": map[string]any{"type": "string"}, "sent_at": nullableTime, "sender": map[string]any{"type": "string"}, "recipients": stringArray, "automated_class": map[string]any{"type": "string"}, "list_id": map[string]any{"type": "string"}, "conversation": map[string]any{"$ref": "#/$defs/conversation_summary"},
	}
	required := []string{"ref", "doc_id", "conversation_ref", "conversation_id", "conversation_method", "subject", "sent_at", "sender", "recipients"}
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
	summary := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"conversation_ref": map[string]any{"type": "string"}, "conversation_id": map[string]any{"type": "string"}, "method": map[string]any{"type": "string"}, "message_count": map[string]any{"type": "integer", "minimum": 0}, "matched_count": map[string]any{"type": "integer", "minimum": 0}, "first_sent_at": nullableTime, "last_sent_at": nullableTime, "participants": stringArray,
	}, "required": []string{"conversation_ref", "conversation_id", "method", "message_count", "matched_count", "first_sent_at", "last_sent_at", "participants"}}
	message := mailboxMessageSchema(nil, nullableTime, stringArray)
	relationship := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"method": map[string]any{"type": "string"}, "parent_doc_id": map[string]any{"type": "string"}, "parent_ref": map[string]any{"type": "string"}}, "required": []string{"method"}}
	candidateEvent := mailboxMessageSchema(map[string]any{"relationship": map[string]any{"$ref": "#/$defs/relationship"}}, nullableTime, stringArray)
	return map[string]any{
		"budget":  map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"used": map[string]any{"type": "integer", "minimum": 1}, "allowed": map[string]any{"const": targetToolResultBytes}, "unit": map[string]any{"const": utf8ByteUnit}}, "required": []string{"used", "allowed", "unit"}},
		"warning": warning, "omission": omission, "conversation_summary": summary, "message": message, "relationship": relationship, "candidate_event": candidateEvent,
		"list_result": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
			"messages": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/message"}}, "filters": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"sender": map[string]any{"type": "string"}, "sender_domain": map[string]any{"type": "string"}, "recipient": map[string]any{"type": "string"}, "after": nullableTime, "before": nullableTime, "collapse_conversations": map[string]any{"type": "boolean"}, "direction": map[string]any{"type": "string"}}, "required": []string{"collapse_conversations"}}, "order": map[string]any{"enum": []string{"newest_first", "oldest_first"}}, "page": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"limit": map[string]any{"type": "integer", "minimum": 1}, "returned": map[string]any{"type": "integer", "minimum": 0}, "has_more": map[string]any{"type": "boolean"}, "next_cursor": map[string]any{"type": "string"}, "snapshot": map[string]any{"type": "string", "format": "date-time"}}, "required": []string{"limit", "returned", "has_more", "snapshot"}}, "omissions": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/omission"}}, "warnings": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/warning"}},
		}, "required": []string{"messages", "filters", "order", "page", "omissions", "warnings"}},
		"conversation_result": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
			"conversation_ref": map[string]any{"type": "string", "minLength": 1}, "conversation_id": map[string]any{"type": "string", "minLength": 1}, "method": map[string]any{"type": "string"}, "message_count": map[string]any{"type": "integer", "minimum": 1}, "participants": stringArray, "messages": map[string]any{"type": "array", "items": mailboxMessageSchema(map[string]any{"relationship": map[string]any{"$ref": "#/$defs/relationship"}}, nullableTime, stringArray)}, "snapshot": map[string]any{"type": "string", "format": "date-time"}, "omissions": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/omission"}}, "warnings": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/warning"}},
		}, "required": []string{"conversation_ref", "conversation_id", "method", "message_count", "participants", "messages", "snapshot", "omissions", "warnings"}},
		"awaiting_reply_result": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
			"candidates": map[string]any{"type": "array", "items": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"conversation_ref": map[string]any{"type": "string"}, "conversation_id": map[string]any{"type": "string"}, "conversation_method": map[string]any{"type": "string"}, "latest_inbound": map[string]any{"$ref": "#/$defs/candidate_event"}, "later_events": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/candidate_event"}}, "participants": stringArray, "first_sent_at": nullableTime, "last_sent_at": nullableTime, "warnings": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/warning"}}}, "required": []string{"conversation_ref", "conversation_id", "conversation_method", "latest_inbound", "later_events", "participants", "warnings"}}}, "filters": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"participant": map[string]any{"type": "string"}, "after": nullableTime, "before": nullableTime}}, "limit": map[string]any{"type": "integer", "minimum": 1}, "snapshot": map[string]any{"type": "string", "format": "date-time"}, "omissions": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/omission"}}, "warnings": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/warning"}},
		}, "required": []string{"candidates", "filters", "limit", "snapshot", "omissions", "warnings"}},
	}
}
