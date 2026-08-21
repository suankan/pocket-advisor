package mcp

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func strPtr(s string) *string { return &s }

func TestValidateFetchDocumentArgumentsRequiresExactlyOneMode(t *testing.T) {
	for name, args := range map[string]fetchDocumentArguments{
		"none set":             {},
		"filename and sender":  {Filename: "a.pdf", Sender: "ada@example.test", Date: strPtr("2026-01-07")},
		"sender and subject":   {Sender: "ada@example.test", Subject: "Review", Date: strPtr("2026-01-07")},
		"filename and subject": {Filename: "a.pdf", Subject: "Review", Date: strPtr("2026-01-07")},
		"all three":            {Filename: "a.pdf", Sender: "ada@example.test", Subject: "Review", Date: strPtr("2026-01-07")},
	} {
		if _, err := validateFetchDocumentArguments(args); err == nil {
			t.Errorf("%s: unexpectedly accepted", name)
		} else {
			var invalid *argumentError
			if !errors.As(err, &invalid) {
				t.Errorf("%s: error = %T, want argumentError", name, err)
			}
		}
	}
}

func TestValidateFetchDocumentArgumentsFilenameRejectsDate(t *testing.T) {
	_, err := validateFetchDocumentArguments(fetchDocumentArguments{Filename: "a.pdf", Date: strPtr("2026-01-07")})
	if err == nil {
		t.Fatal("filename with date unexpectedly accepted")
	}
}

func TestValidateFetchDocumentArgumentsSenderAndSubjectRequireDate(t *testing.T) {
	if _, err := validateFetchDocumentArguments(fetchDocumentArguments{Sender: "ada@example.test"}); err == nil {
		t.Error("sender without date unexpectedly accepted")
	}
	if _, err := validateFetchDocumentArguments(fetchDocumentArguments{Subject: "Review"}); err == nil {
		t.Error("subject without date unexpectedly accepted")
	}
	if _, err := validateFetchDocumentArguments(fetchDocumentArguments{Sender: "ada@example.test", Date: strPtr("")}); err == nil {
		t.Error("sender with a blank date unexpectedly accepted")
	}
}

func TestValidateFetchDocumentArgumentsRejectsMalformedDate(t *testing.T) {
	_, err := validateFetchDocumentArguments(fetchDocumentArguments{Sender: "ada@example.test", Date: strPtr("not-a-date")})
	if err == nil {
		t.Fatal("malformed date unexpectedly accepted")
	}
}

func TestValidateFetchDocumentArgumentsRejectsOverLongFields(t *testing.T) {
	if _, err := validateFetchDocumentArguments(fetchDocumentArguments{Filename: strings.Repeat("a", maxDocumentFilenameRunes+1)}); err == nil {
		t.Error("over-long filename unexpectedly accepted")
	}
	if _, err := validateFetchDocumentArguments(fetchDocumentArguments{Subject: strings.Repeat("a", maxDocumentSubjectRunes+1), Date: strPtr("2026-01-07")}); err == nil {
		t.Error("over-long subject unexpectedly accepted")
	}
	if _, err := validateFetchDocumentArguments(fetchDocumentArguments{Sender: strings.Repeat("a", maxMailboxAddressRunes+1), Date: strPtr("2026-01-07")}); err == nil {
		t.Error("over-long sender unexpectedly accepted")
	}
}

func TestValidateFetchDocumentArgumentsAcceptsEachMode(t *testing.T) {
	cases := []struct {
		name string
		args fetchDocumentArguments
		want fetchDocumentMode
	}{
		{"filename", fetchDocumentArguments{Filename: "invoice.pdf"}, fetchByFilename},
		{"sender+date", fetchDocumentArguments{Sender: "ada@example.test", Date: strPtr("2026-01-07")}, fetchBySenderDate},
		{"subject+date", fetchDocumentArguments{Subject: "Quarterly review", Date: strPtr("2026-01-07T09:00:00Z")}, fetchBySubjectDate},
	}
	for _, c := range cases {
		mode, err := validateFetchDocumentArguments(c.args)
		if err != nil {
			t.Errorf("%s: unexpectedly rejected: %v", c.name, err)
		}
		if mode != c.want {
			t.Errorf("%s: mode = %q, want %q", c.name, mode, c.want)
		}
	}
}

func TestBoundDocumentTextLeavesShortTextUntouched(t *testing.T) {
	text, truncated := boundDocumentText("short body")
	if truncated || text != "short body" {
		t.Errorf("got (%q, %v), want the text unchanged and untruncated", text, truncated)
	}
}

// A body past the bound is truncated at a UTF-8 boundary — never mid
// multi-byte rune — and flagged.
func TestBoundDocumentTextTruncatesAtUTF8Boundary(t *testing.T) {
	rune3 := "あ"                                        // 3 UTF-8 bytes, well past any 1-byte boundary math would assume
	long := strings.Repeat(rune3, maxDocumentTextBytes) // far larger than the byte bound
	text, truncated := boundDocumentText(long)
	if !truncated {
		t.Fatal("want truncated=true for a body past the bound")
	}
	if len(text) > maxDocumentTextBytes {
		t.Errorf("truncated text = %d bytes, want at most %d", len(text), maxDocumentTextBytes)
	}
	if !isValidUTF8(text) {
		t.Errorf("truncated text is not valid UTF-8: %q", text)
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

// renderDocumentResult and finalizeDocumentResult stay within the shared MCP
// response bounds and produce structuredContent that matches the tool's own
// published outputSchema — the same contract every other tool's finalize path
// (finalizeMailboxResult, the query result path) is held to.
func TestFinalizeDocumentResultIsBoundedAndSchemaValid(t *testing.T) {
	date := time.Date(2026, 1, 7, 9, 0, 0, 0, time.UTC).Format(time.RFC3339)
	result := &DocumentFetchResult{
		Snapshot: time.Now().UTC(),
		Documents: []FetchedDocument{{
			DocID: "11111111-1111-1111-1111-111111111111", DocType: "email", MimeType: "message/rfc822",
			SourceFilename: "message.eml", Subject: "Quarterly review", From: "ada@example.test",
			To: []string{"bob@example.test"}, Cc: []string{"carol@example.test"}, Date: &date,
			SourcePath: "workspaces/data/main/emails/message.eml", Text: "Hello", TextTruncated: false,
			Attachments: []AttachmentDocument{{
				DocID: "22222222-2222-2222-2222-222222222222", DocType: "pdf", MimeType: "application/pdf",
				SourceFilename: "attachment.pdf", ContainerSourcePath: "workspaces/data/main/emails/message.eml",
			}},
		}},
	}
	toolResult, err := finalizeDocumentResult(result)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if toolResult.IsError {
		t.Fatalf("unexpected tool error: %+v", toolResult)
	}
	encoded, err := json.Marshal(toolResult)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > targetToolResultBytes {
		t.Errorf("encoded result = %d bytes, target = %d", len(encoded), targetToolResultBytes)
	}
	if lines := readableLines(toolResult.Content[0].Text); lines > targetReadableLines {
		t.Errorf("readable result = %d lines, target = %d", lines, targetReadableLines)
	}

	page, ok := toolResult.StructuredContent.(*documentResult)
	if !ok {
		t.Fatalf("structured content type = %T", toolResult.StructuredContent)
	}
	schema := compileJSONSchema(t, "fetch-document.schema.json", documentOutputSchema())
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	var instance any
	if err := json.Unmarshal(raw, &instance); err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(instance); err != nil {
		t.Fatalf("structuredContent does not match outputSchema: %v", err)
	}

	fallback := toolResult.Content[0].Text
	for _, want := range []string{"Quarterly review", "ada@example.test", "1 attachment", "workspaces/data/main/emails/message.eml"} {
		if !strings.Contains(fallback, want) {
			t.Errorf("text fallback should contain %q: %s", want, fallback)
		}
	}
}

// A document without an email subject renders by filename instead, and a
// truncated body is flagged in the human-readable text, not only in
// structuredContent.
func TestRenderDocumentResultNonEmailUsesFilenameAndFlagsTruncation(t *testing.T) {
	page := &documentResult{Kind: "document_fetch", Result: &DocumentFetchResult{
		Snapshot: time.Now().UTC(),
		Documents: []FetchedDocument{{
			DocID: "11111111-1111-1111-1111-111111111111", DocType: "pdf", MimeType: "application/pdf",
			SourceFilename: "invoice.pdf", SourcePath: "workspaces/data/main/invoice.pdf",
			Text: "body", TextTruncated: true,
		}},
	}, ResponseBudget: ResponseBudget{Allowed: targetToolResultBytes, Unit: utf8ByteUnit}}
	text := renderDocumentResult(page)
	for _, want := range []string{"invoice.pdf", "workspaces/data/main/invoice.pdf", "truncated"} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered text should contain %q: %s", want, text)
		}
	}
}

// The published input schema accepts each advertised shape and still rejects
// any property outside the closed set fetch_document actually reads.
func TestDocumentInputSchemaIsClosed(t *testing.T) {
	schema := compileJSONSchema(t, "fetch-document-input.schema.json", documentInputSchema())
	for name, valid := range map[string]any{
		"filename":    map[string]any{"filename": "invoice.pdf"},
		"sender+date": map[string]any{"sender": "ada@example.test", "date": "2026-01-07"},
		"subject":     map[string]any{"subject": "Quarterly review", "date": "2026-01-07T09:00:00Z"},
	} {
		if err := schema.Validate(valid); err != nil {
			t.Errorf("%s: unexpectedly rejected by the schema: %v", name, err)
		}
	}
	if err := schema.Validate(map[string]any{"filename": "invoice.pdf", "workspace": "other"}); err == nil {
		t.Error("an unadvertised property was unexpectedly accepted")
	}
}
