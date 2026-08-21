package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/suankan/pocket-advisor/internal/storage/postgres"
)

const (
	maxDocumentSubjectRunes  = 512
	maxDocumentFilenameRunes = 512
	// maxDocumentTextBytes bounds one matched document's body in the
	// response. This tool answers "what is this specific document", not "let
	// me page through it" — search/read_evidence already own bounded,
	// continuable text delivery, so a body past this bound is truncated
	// rather than given its own cursor.
	maxDocumentTextBytes = 32 * 1024
)

// DocumentTool exposes exact, deterministic document lookup by attributes
// the other fixed-workspace tools already return: a message's sender and
// date, its subject and date, or any document's original filename. Where
// list_messages and search return references to browse or rank by,
// DocumentTool answers "what is this one document" completely in a single
// call — its location, its email attribution when it has one, its body text,
// and its attachments — using only values a caller already has in hand.
type DocumentTool struct {
	Docs      *postgres.DocumentRepo
	Workspace string
	Title     string
	// Log traces every fetch_document call. Nil-safe, see QueryTool.Log.
	Log *slog.Logger
}

func (t *DocumentTool) logger() *slog.Logger {
	if t.Log != nil {
		return t.Log
	}
	return slog.Default()
}

func (t *DocumentTool) Name() string { return "fetch_document" }

func (t *DocumentTool) Describe() ToolDefinition {
	title := t.Title
	if title == "" {
		title = t.Workspace
	}
	return ToolDefinition{
		Name:  t.Name(),
		Title: "Fetch a " + title + " document",
		Description: "Deterministically fetch one or more documents in this fixed workspace by an exact " +
			"attribute, not a similarity search. Pass exactly one of: filename (a document's original " +
			"filename, exactly as list_messages, search, or an earlier fetch_document call reported it); " +
			"sender plus date (an email's exact normalized sender mailbox and the day it was sent); or " +
			"subject plus date (an email's exact subject text and the day it was sent). date accepts an " +
			"RFC 3339 timestamp or a YYYY-MM-DD date and is always matched by day.\n\n" +
			"Every match reports where to find it, exactly as search and the mailbox tools do: " +
			"source_path is the document's own staged file, and container_source_path names the file it " +
			"was extracted from when it has none of its own. Report the path given; never infer one from " +
			"where similar documents live. An email's from/to/cc and its body text are included, truncated " +
			"and flagged text_truncated=true past a fixed size. Attachments are listed with their own " +
			"location but not their text; fetch one by its filename to read it.\n\n" +
			"If more than one document matches, every match is returned rather than a guess at which one " +
			"was meant — narrow with a more exact subject or date. Zero matches means the attribute or " +
			"date was not exact; do not fall back to search results for a different message and present " +
			"them as this one.",
		InputSchema: documentInputSchema(), OutputSchema: documentOutputSchema(),
		Annotations: readOnlyAnnotations(),
	}
}

type fetchDocumentArguments struct {
	Sender   string  `json:"sender,omitempty"`
	Subject  string  `json:"subject,omitempty"`
	Filename string  `json:"filename,omitempty"`
	Date     *string `json:"date,omitempty"`
}

// Call validates and dispatches fetch_document. QueryTool calls it before its
// retrieval dispatcher, matching how MailboxTool and TimelineTool are wired.
func (t *DocumentTool) Call(ctx context.Context, raw json.RawMessage) (CallToolResult, error) {
	if t == nil || t.Docs == nil || strings.TrimSpace(t.Workspace) == "" {
		return CallToolResult{}, fmt.Errorf("document lookup service is unavailable")
	}
	var params rawCallParams
	if err := decodeStrict(raw, &params); err != nil {
		return CallToolResult{}, &argumentError{message: "tools/call params must be a valid object"}
	}
	if params.Name != t.Name() {
		return CallToolResult{}, &unknownToolError{}
	}
	if len(params.Task) > 0 && string(params.Task) != "null" {
		return CallToolResult{}, &argumentError{message: "task-augmented execution is not supported"}
	}
	var args fetchDocumentArguments
	if err := decodeStrict(params.Arguments, &args); err != nil {
		return CallToolResult{}, &argumentError{message: "arguments must match the advertised fetch_document input schema"}
	}

	mode, err := validateFetchDocumentArguments(args)
	if err != nil {
		return CallToolResult{}, err
	}

	var docs []postgres.DocumentLocation
	switch mode {
	case fetchByFilename:
		docs, err = t.Docs.FindByFilename(ctx, args.Filename)
	case fetchBySenderDate:
		day, _ := parseMailboxDate(args.Date)
		docs, err = t.Docs.FindEmailsBySenderDate(ctx, normalizeExactAddress(args.Sender), truncateToDay(day))
	case fetchBySubjectDate:
		day, _ := parseMailboxDate(args.Date)
		docs, err = t.Docs.FindEmailsBySubjectDate(ctx, strings.TrimSpace(args.Subject), truncateToDay(day))
	}
	if err != nil {
		t.logger().Info("fetch_document", "mode", mode, "error", err.Error())
		return CallToolResult{}, fmt.Errorf("fetch document: %w", err)
	}

	result := &DocumentFetchResult{Snapshot: time.Now().UTC()}
	for _, d := range docs {
		fetched, err := t.render(ctx, d)
		if err != nil {
			t.logger().Info("fetch_document", "mode", mode, "error", err.Error())
			return CallToolResult{}, err
		}
		result.Documents = append(result.Documents, fetched)
	}
	t.logger().Info("fetch_document", "mode", mode, "matches", len(result.Documents))
	return finalizeDocumentResult(result)
}

type fetchDocumentMode string

const (
	fetchByFilename     fetchDocumentMode = "filename"
	fetchBySenderDate   fetchDocumentMode = "sender_date"
	fetchBySubjectDate  fetchDocumentMode = "subject_date"
	fetchDocumentNoMode fetchDocumentMode = ""
)

// validateFetchDocumentArguments enforces exactly one closed lookup mode.
// Accepting several loosely-related fields and letting the store figure out
// which ones mattered is how "date without sender or subject" silently
// becomes a full-corpus date scan; this rejects every combination but the
// three the tool advertises.
func validateFetchDocumentArguments(args fetchDocumentArguments) (fetchDocumentMode, error) {
	sender := strings.TrimSpace(args.Sender)
	subject := strings.TrimSpace(args.Subject)
	filename := strings.TrimSpace(args.Filename)

	set := 0
	var mode fetchDocumentMode
	if filename != "" {
		set++
		mode = fetchByFilename
	}
	if sender != "" {
		set++
		mode = fetchBySenderDate
	}
	if subject != "" {
		set++
		mode = fetchBySubjectDate
	}
	if set != 1 {
		return fetchDocumentNoMode, &argumentError{message: "pass exactly one of filename, sender+date, or subject+date"}
	}
	if len(filename) > maxDocumentFilenameRunes {
		return fetchDocumentNoMode, &argumentError{message: "filename exceeds the bounded length"}
	}
	if err := validateMailboxAddress(sender, "sender"); err != nil {
		return fetchDocumentNoMode, err
	}
	if utf8.RuneCountInString(subject) > maxDocumentSubjectRunes {
		return fetchDocumentNoMode, &argumentError{message: "subject exceeds the bounded length"}
	}
	if mode == fetchByFilename {
		if args.Date != nil {
			return fetchDocumentNoMode, &argumentError{message: "date is not accepted with filename"}
		}
		return mode, nil
	}
	if args.Date == nil || strings.TrimSpace(*args.Date) == "" {
		return fetchDocumentNoMode, &argumentError{message: "date is required with sender or subject"}
	}
	if _, err := parseMailboxDate(args.Date); err != nil {
		return fetchDocumentNoMode, err
	}
	return mode, nil
}

func normalizeExactAddress(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func truncateToDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// FetchedDocument is one matched document, complete: location, email
// attribution when it has one, body text, and its direct attachments.
type FetchedDocument struct {
	DocID               string               `json:"doc_id"`
	ParentDocID         string               `json:"parent_doc_id,omitempty"`
	DocType             string               `json:"doc_type"`
	MimeType            string               `json:"mime_type"`
	SourceFilename      string               `json:"source_filename"`
	Subject             string               `json:"subject,omitempty"`
	From                string               `json:"from,omitempty"`
	To                  []string             `json:"to,omitempty"`
	Cc                  []string             `json:"cc,omitempty"`
	Date                *string              `json:"date,omitempty"`
	SourcePath          string               `json:"source_path,omitempty"`
	ContainerSourcePath string               `json:"container_source_path,omitempty"`
	Text                string               `json:"text,omitempty"`
	TextTruncated       bool                 `json:"text_truncated"`
	Attachments         []AttachmentDocument `json:"attachments,omitempty"`
}

// AttachmentDocument is a direct child document: identified and located, but
// without its own body text — fetch it by filename for that.
type AttachmentDocument struct {
	DocID               string `json:"doc_id"`
	DocType             string `json:"doc_type"`
	MimeType            string `json:"mime_type"`
	SourceFilename      string `json:"source_filename"`
	SourcePath          string `json:"source_path,omitempty"`
	ContainerSourcePath string `json:"container_source_path,omitempty"`
}

type DocumentFetchResult struct {
	Documents []FetchedDocument `json:"documents"`
	Snapshot  time.Time         `json:"snapshot"`
}

func (t *DocumentTool) render(ctx context.Context, d postgres.DocumentLocation) (FetchedDocument, error) {
	fetched := FetchedDocument{
		DocID: d.DocID, ParentDocID: d.ParentDocID, DocType: d.DocType, MimeType: d.MimeType,
		SourceFilename: d.SourceFilename, SourcePath: d.SourcePath, ContainerSourcePath: d.ContainerPath,
	}
	if d.DocType == "email" {
		fetched.Subject = d.EmailSubject
		fetched.From = d.EmailFrom
		if d.EmailDate != nil {
			formatted := d.EmailDate.UTC().Format(time.RFC3339)
			fetched.Date = &formatted
		}
		to, cc, err := t.Docs.EmailRecipients(ctx, d.DocID)
		if err != nil {
			return FetchedDocument{}, err
		}
		fetched.To, fetched.Cc = to, cc
	}
	fetched.Text, fetched.TextTruncated = boundDocumentText(d.NormalizedText)

	children, err := t.Docs.Children(ctx, d.DocID)
	if err != nil {
		return FetchedDocument{}, err
	}
	for _, c := range children {
		fetched.Attachments = append(fetched.Attachments, AttachmentDocument{
			DocID: c.DocID, DocType: c.DocType, MimeType: c.MimeType, SourceFilename: c.SourceFilename,
			SourcePath: c.SourcePath, ContainerSourcePath: c.ContainerPath,
		})
	}
	return fetched, nil
}

func boundDocumentText(text string) (string, bool) {
	if len(text) <= maxDocumentTextBytes {
		return text, false
	}
	return text[:utf8BoundaryAtOrBefore(text, maxDocumentTextBytes)], true
}

type documentResult struct {
	Kind           string         `json:"kind"`
	Result         any            `json:"result"`
	ResponseBudget ResponseBudget `json:"response_budget"`
}

// finalizeDocumentResult applies the same budget/size/line bounds as
// finalizeMailboxResult (internal/mcp/mailbox.go): a fixed-point pass to
// measure the response's own encoded size accurately, then the shared MCP
// response ceilings.
func finalizeDocumentResult(value *DocumentFetchResult) (CallToolResult, error) {
	page := &documentResult{Kind: "document_fetch", Result: value, ResponseBudget: ResponseBudget{Allowed: targetToolResultBytes, Unit: utf8ByteUnit}}
	var result CallToolResult
	for range 4 {
		text := renderDocumentResult(page)
		result = CallToolResult{Content: []TextContent{{Type: "text", Text: text}}, StructuredContent: page, IsError: false}
		encoded, err := json.Marshal(result)
		if err != nil {
			return CallToolResult{}, fmt.Errorf("encode document result: %w", err)
		}
		if page.ResponseBudget.Used == len(encoded) {
			break
		}
		page.ResponseBudget.Used = len(encoded)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return CallToolResult{}, fmt.Errorf("encode document result: %w", err)
	}
	if len(encoded) > targetToolResultBytes {
		return CallToolResult{}, &resultSizeError{limit: targetToolResultBytes}
	}
	if readableLines(result.Content[0].Text) > targetReadableLines {
		return CallToolResult{}, &resultLineError{limit: targetReadableLines}
	}
	return result, nil
}

func renderDocumentResult(page *documentResult) string {
	result, ok := page.Result.(*DocumentFetchResult)
	if !ok {
		return "Document fetch result."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Document fetch: %d match(es).\n", len(result.Documents))
	for _, d := range result.Documents {
		switch {
		case d.Subject != "":
			fmt.Fprintf(&b, "[%s] %s — %s", d.DocID, d.Subject, d.From)
		default:
			fmt.Fprintf(&b, "[%s] %s", d.DocID, d.SourceFilename)
		}
		if d.Date != nil {
			fmt.Fprintf(&b, " (%s)", *d.Date)
		}
		b.WriteString("\n")
		switch {
		case d.SourcePath != "":
			fmt.Fprintf(&b, "  file: %s\n", d.SourcePath)
		case d.ContainerSourcePath != "":
			fmt.Fprintf(&b, "  extracted from: %s\n", d.ContainerSourcePath)
		}
		if len(d.Attachments) > 0 {
			fmt.Fprintf(&b, "  %d attachment(s)\n", len(d.Attachments))
		}
		if d.TextTruncated {
			b.WriteString("  text truncated to the response size bound\n")
		}
	}
	fmt.Fprintf(&b, "Snapshot: %s. Response budget: %d of %d UTF-8 bytes.\n", result.Snapshot.Format(time.RFC3339), page.ResponseBudget.Used, page.ResponseBudget.Allowed)
	return b.String()
}

func documentInputSchema() map[string]any {
	return map[string]any{
		"$schema": jsonSchema202012, "type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"sender":   map[string]any{"type": "string", "maxLength": maxMailboxAddressRunes, "description": "One exact normalized sender mailbox. Requires date."},
			"subject":  map[string]any{"type": "string", "maxLength": maxDocumentSubjectRunes, "description": "One exact email subject. Requires date."},
			"filename": map[string]any{"type": "string", "maxLength": maxDocumentFilenameRunes, "description": "One exact original document filename. Not combined with date."},
			"date":     nullableDateSchema(),
		},
	}
}

func documentOutputSchema() map[string]any {
	stringArray := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	attachment := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"doc_id": map[string]any{"type": "string", "minLength": 1}, "doc_type": map[string]any{"type": "string"}, "mime_type": map[string]any{"type": "string"},
		"source_filename": map[string]any{"type": "string"}, "source_path": map[string]any{"type": "string"}, "container_source_path": map[string]any{"type": "string"},
	}, "required": []string{"doc_id", "doc_type", "mime_type", "source_filename"}}
	document := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"doc_id": map[string]any{"type": "string", "minLength": 1}, "parent_doc_id": map[string]any{"type": "string"},
		"doc_type": map[string]any{"type": "string"}, "mime_type": map[string]any{"type": "string"}, "source_filename": map[string]any{"type": "string"},
		"subject": map[string]any{"type": "string"}, "from": map[string]any{"type": "string"}, "to": stringArray, "cc": stringArray,
		"date": map[string]any{"type": "string", "format": "date-time"}, "source_path": map[string]any{"type": "string"}, "container_source_path": map[string]any{"type": "string"},
		"text": map[string]any{"type": "string"}, "text_truncated": map[string]any{"type": "boolean"},
		"attachments": map[string]any{"type": "array", "items": attachment},
	}, "required": []string{"doc_id", "doc_type", "mime_type", "source_filename", "text_truncated"}}
	return map[string]any{
		"$schema": jsonSchema202012, "type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"kind": map[string]any{"const": "document_fetch"},
			"result": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
				"documents": map[string]any{"type": "array", "items": document}, "snapshot": map[string]any{"type": "string", "format": "date-time"},
			}, "required": []string{"documents", "snapshot"}},
			"response_budget": map[string]any{"$ref": "#/$defs/budget"},
		},
		"required": []string{"kind", "result", "response_budget"},
		"$defs":    map[string]any{"budget": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"used": map[string]any{"type": "integer", "minimum": 1}, "allowed": map[string]any{"const": targetToolResultBytes}, "unit": map[string]any{"const": utf8ByteUnit}}, "required": []string{"used", "allowed", "unit"}}},
	}
}
