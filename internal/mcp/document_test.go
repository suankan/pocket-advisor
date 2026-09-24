package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

const testFingerprint = "0123456789abcdef"

func testDocID(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

func emailMatch(n int, text string) documentMatch {
	date := time.Date(2026, 1, 7, 9, 0, 0, 0, time.UTC).Format(time.RFC3339)
	header := FetchedDocument{
		DocID: testDocID(n), DocType: "email", MimeType: "message/rfc822",
		SourceFilename: "message.eml", Subject: "Quarterly review", From: "ada@example.test",
		To: []string{"bob@example.test"}, Cc: []string{"carol@example.test"}, Date: &date,
		SourcePath: "emails/message.eml", SourceRef: "source:" + testDocID(n),
	}
	match := documentMatch{header: header, text: text, attachments: []AttachmentDocument{{
		DocID: testDocID(1000 + n), DocType: "pdf", MimeType: "application/pdf",
		SourceFilename: "attachment.pdf", ContainerSourcePath: "emails/message.eml",
	}}}
	match.header.TextLength = len(text)
	match.header.AttachmentCount = len(match.attachments)
	return match
}

// pageThrough follows every cursor from the first page, checks each page
// against the response bounds and the published output schema, and returns
// the pages in order.
func pageThrough(t *testing.T, matches []documentMatch) []*documentResult {
	t.Helper()
	schema := compileJSONSchema(t, "fetch-document.schema.json", documentOutputSchema())
	var pages []*documentResult
	pos := documentPosition{phase: phaseText}
	for range 1000 {
		result, next, err := buildDocumentPage(matches, false, time.Now().UTC(), pos, testFingerprint)
		if err != nil {
			t.Fatalf("page %d: %v", len(pages)+1, err)
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded) > targetToolResultBytes {
			t.Fatalf("page %d = %d bytes, target %d", len(pages)+1, len(encoded), targetToolResultBytes)
		}
		if lines := readableLines(result.Content[0].Text); lines > targetReadableLines {
			t.Fatalf("page %d = %d lines, target %d", len(pages)+1, lines, targetReadableLines)
		}
		page := result.StructuredContent.(*documentResult)
		var instance any
		raw, _ := json.Marshal(page)
		if err := json.Unmarshal(raw, &instance); err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(instance); err != nil {
			t.Fatalf("page %d does not match outputSchema: %v", len(pages)+1, err)
		}
		pages = append(pages, page)
		if next == nil {
			if !page.Complete || page.NextCursor != nil || page.ContinuationTool != nil {
				t.Fatalf("final page continuation = %v %v %v", page.Complete, page.NextCursor, page.ContinuationTool)
			}
			return pages
		}
		if page.Complete || page.NextCursor == nil || *page.ContinuationTool != "fetch_document" {
			t.Fatalf("page %d does not continue: %+v", len(pages), page)
		}
		cursor, err := decodeDocumentCursor(*page.NextCursor, testFingerprint)
		if err != nil {
			t.Fatalf("decode issued cursor: %v", err)
		}
		if pos, err = cursor.position(matches); err != nil {
			t.Fatalf("resolve issued cursor: %v", err)
		}
	}
	t.Fatal("paging did not terminate")
	return nil
}

// reassemble concatenates each match's delivered text and attachments,
// checking the ranges are contiguous from zero.
func reassemble(t *testing.T, pages []*documentResult) (map[string]string, map[string][]AttachmentDocument) {
	t.Helper()
	texts := map[string]string{}
	attachments := map[string][]AttachmentDocument{}
	for _, page := range pages {
		for _, d := range page.Result.(*DocumentFetchResult).Documents {
			if d.TextRange != nil {
				if d.TextRange.Start != len(texts[d.DocID]) || d.TextRange.End-d.TextRange.Start != len(d.Text) {
					t.Fatalf("%s: range %+v does not continue %d delivered bytes", d.DocID, *d.TextRange, len(texts[d.DocID]))
				}
				if !utf8.ValidString(d.Text) {
					t.Fatalf("%s: text slice is not valid UTF-8", d.DocID)
				}
				texts[d.DocID] += d.Text
				if d.TextComplete != (d.TextRange.End == d.TextLength) {
					t.Fatalf("%s: text_complete=%v at %d of %d", d.DocID, d.TextComplete, d.TextRange.End, d.TextLength)
				}
			}
			if len(d.Attachments) > 0 {
				if d.AttachmentsOffset != len(attachments[d.DocID]) {
					t.Fatalf("%s: attachments_offset %d does not continue %d delivered", d.DocID, d.AttachmentsOffset, len(attachments[d.DocID]))
				}
				attachments[d.DocID] = append(attachments[d.DocID], d.Attachments...)
			}
		}
	}
	return texts, attachments
}

// A small match is one complete, schema-valid page carrying every field.
func TestDocumentPageSmallMatchIsCompleteAndReadable(t *testing.T) {
	pages := pageThrough(t, []documentMatch{emailMatch(1, "Hello")})
	if len(pages) != 1 {
		t.Fatalf("pages = %d, want 1", len(pages))
	}
	d := pages[0].Result.(*DocumentFetchResult).Documents[0]
	if d.Text != "Hello" || !d.TextComplete || len(d.Attachments) != 1 {
		t.Fatalf("document = %+v", d)
	}
}

// A body far past one page is delivered whole across pages — the defect this
// replaces failed any body past roughly 23 KiB outright.
func TestDocumentPageLargeTextPagesToTheExactOriginal(t *testing.T) {
	var b strings.Builder
	for i := 0; b.Len() < 200*1024; i++ {
		fmt.Fprintf(&b, "Paragraph %d — «многобайтовый» текст, あいう, \"quoted\" and\ttabbed.\n\n", i)
	}
	text := b.String()
	match := emailMatch(1, text)
	pages := pageThrough(t, []documentMatch{match})
	if len(pages) < 5 {
		t.Fatalf("pages = %d, want the body split across several", len(pages))
	}
	texts, attachments := reassemble(t, pages)
	if texts[match.header.DocID] != text {
		t.Fatal("reassembled text differs from the original")
	}
	if len(attachments[match.header.DocID]) != 1 {
		t.Fatalf("attachments = %d, want 1", len(attachments[match.header.DocID]))
	}
}

// Many large matches are all delivered, in order, each in full.
func TestDocumentPageManyLargeMatchesAreAllDelivered(t *testing.T) {
	var matches []documentMatch
	for i := range 20 {
		matches = append(matches, emailMatch(i, strings.Repeat(fmt.Sprintf("match %d line\n", i), 3000)))
	}
	pages := pageThrough(t, matches)
	texts, _ := reassemble(t, pages)
	for _, m := range matches {
		if texts[m.header.DocID] != m.text {
			t.Fatalf("%s: delivered %d of %d bytes", m.header.DocID, len(texts[m.header.DocID]), len(m.text))
		}
	}
	var order []string
	for _, page := range pages {
		for _, d := range page.Result.(*DocumentFetchResult).Documents {
			if len(order) == 0 || order[len(order)-1] != d.DocID {
				order = append(order, d.DocID)
			}
		}
	}
	if len(order) != len(matches) {
		t.Fatalf("match order = %v", order)
	}
}

// A container with hundreds of children lists every one across pages.
func TestDocumentPageListsEveryAttachment(t *testing.T) {
	match := emailMatch(1, "Archive body")
	match.attachments = nil
	for i := range 600 {
		match.attachments = append(match.attachments, AttachmentDocument{
			DocID: testDocID(2000 + i), DocType: "pdf", MimeType: "application/pdf",
			SourceFilename: fmt.Sprintf("member-%03d.pdf", i), ContainerSourcePath: "archives/bundle.zip",
		})
	}
	match.header.AttachmentCount = len(match.attachments)
	pages := pageThrough(t, []documentMatch{match})
	_, attachments := reassemble(t, pages)
	got := attachments[match.header.DocID]
	if len(got) != 600 || got[0].SourceFilename != "member-000.pdf" || got[599].SourceFilename != "member-599.pdf" {
		t.Fatalf("attachments delivered = %d", len(got))
	}
}

// A mass mailing's recipients are bounded and the remainder counted.
func TestBoundRecipientsCountsTheRemainder(t *testing.T) {
	var many []string
	for i := range 419 {
		many = append(many, fmt.Sprintf("r%d@example.test", i))
	}
	kept, omitted := boundRecipients(many)
	if len(kept) != maxDocumentRecipients || omitted != 419-maxDocumentRecipients {
		t.Fatalf("kept %d, omitted %d", len(kept), omitted)
	}
	match := emailMatch(1, "body")
	match.header.To, match.header.ToOmitted = kept, omitted
	pages := pageThrough(t, []documentMatch{match})
	if !strings.Contains(renderDocumentResult(pages[0]), fmt.Sprintf("and %d more", omitted)) {
		t.Fatal("readable text does not report the omitted recipients")
	}
}

// A cursor resumes only the lookup and revision it was issued for.
func TestDocumentCursorRefusesAnotherLookupOrRevision(t *testing.T) {
	matches := []documentMatch{emailMatch(1, strings.Repeat("word ", 30000))}
	_, next, err := buildDocumentPage(matches, false, time.Now().UTC(), documentPosition{phase: phaseText}, testFingerprint)
	if err != nil || next == nil {
		t.Fatalf("first page: next=%v err=%v", next, err)
	}
	cursor, err := encodeDocumentCursor(matches, *next, testFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if len(cursor) > maxCursorBytes {
		t.Fatalf("cursor = %d bytes, bound %d", len(cursor), maxCursorBytes)
	}
	var invalid *argumentError
	if _, err := decodeDocumentCursor(cursor, "fedcba9876543210"); !errors.As(err, &invalid) {
		t.Fatalf("cursor under other arguments: %v", err)
	}
	if _, err := decodeDocumentCursor("not a cursor", testFingerprint); !errors.As(err, &invalid) {
		t.Fatalf("malformed cursor: %v", err)
	}
	decoded, err := decodeDocumentCursor(cursor, testFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	changed := []documentMatch{emailMatch(1, strings.Repeat("other ", 30000))}
	if _, err := decoded.position(changed); !errors.Is(err, errStaleDocumentCursor) {
		t.Fatalf("changed text: %v", err)
	}
	if _, err := decoded.position([]documentMatch{emailMatch(2, matches[0].text)}); !errors.Is(err, errStaleDocumentCursor) {
		t.Fatalf("vanished match: %v", err)
	}
	if pos, err := decoded.position(matches); err != nil || pos != *next {
		t.Fatalf("same revision: pos=%+v err=%v", pos, err)
	}
}

// The lookup fingerprint follows the normalized arguments, so a cursor
// survives cosmetic differences but not a different lookup.
func TestDocumentLookupFingerprint(t *testing.T) {
	a := documentLookupFingerprint(fetchBySenderDate, fetchDocumentArguments{Sender: "Ada@Example.test ", Date: strPtr("2026-01-07T09:00:00Z")})
	b := documentLookupFingerprint(fetchBySenderDate, fetchDocumentArguments{Sender: "ada@example.test", Date: strPtr("2026-01-07")})
	c := documentLookupFingerprint(fetchBySenderDate, fetchDocumentArguments{Sender: "ada@example.test", Date: strPtr("2026-01-08")})
	if a != b || a == c {
		t.Fatalf("fingerprints = %s %s %s", a, b, c)
	}
}

// more_matches reaches both the structured result and the readable text.
func TestDocumentPageReportsMoreMatches(t *testing.T) {
	result, _, err := buildDocumentPage([]documentMatch{emailMatch(1, "body")}, true, time.Now().UTC(), documentPosition{phase: phaseText}, testFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if !result.StructuredContent.(*documentResult).Result.(*DocumentFetchResult).MoreMatches ||
		!strings.Contains(result.Content[0].Text, "More than") {
		t.Fatalf("more_matches not reported: %s", result.Content[0].Text)
	}
}

// The published input schema accepts each advertised shape, with or without
// a cursor, and still rejects any property outside the closed set
// fetch_document actually reads.
func TestDocumentInputSchemaIsClosed(t *testing.T) {
	schema := compileJSONSchema(t, "fetch-document-input.schema.json", documentInputSchema())
	for name, valid := range map[string]any{
		"filename":    map[string]any{"filename": "invoice.pdf"},
		"sender+date": map[string]any{"sender": "ada@example.test", "date": "2026-01-07"},
		"subject":     map[string]any{"subject": "Quarterly review", "date": "2026-01-07T09:00:00Z"},
		"continued":   map[string]any{"filename": "invoice.pdf", "cursor": "opaque"},
	} {
		if err := schema.Validate(valid); err != nil {
			t.Errorf("%s: unexpectedly rejected by the schema: %v", name, err)
		}
	}
	if err := schema.Validate(map[string]any{"filename": "invoice.pdf", "workspace": "other"}); err == nil {
		t.Error("an unadvertised property was unexpectedly accepted")
	}
}
