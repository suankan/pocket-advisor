package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/suankan/pocket-advisor/internal/source"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/suankan/pocket-advisor/internal/storage/postgres"
)

const (
	maxDocumentSubjectRunes  = 512
	maxDocumentFilenameRunes = 512
	// maxDocumentRecipients bounds each of an email's to and cc lists, so
	// one mass mailing's attribution cannot outgrow a response page.
	maxDocumentRecipients = 50
	documentCursorVersion = 1
)

// DocumentTool exposes exact, deterministic document lookup by attributes
// the other fixed-workspace tools already return: a message's sender and
// date, its subject and date, or any document's original filename. Where
// list_messages and search return references to browse or rank by,
// DocumentTool answers "what is this one document" — its location, its email
// attribution when it has one, its body text, and its attachments — using
// only values a caller already has in hand. A result larger than one
// response page continues through a cursor rather than being cut short.
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
			"where similar documents live. An email's from/to/cc, each document's body text, and its " +
			"attachments are included. Attachments are listed with their own location but not their " +
			"text; fetch one by its filename to read it.\n\n" +
			"Results are paged. text_range says which bytes of the body a page carries. When " +
			"complete=false, call this tool again with the same arguments plus exactly next_cursor, and " +
			"continue until complete=true before claiming you have read a whole document. Never " +
			"construct a cursor.\n\n" +
			"If more than one document matches, every match is returned rather than a guess at which one " +
			"was meant — narrow with a more exact subject or date. more_matches=true means more documents " +
			"matched than one lookup returns. Zero matches means the attribute or date was not exact; do " +
			"not fall back to search results for a different message and present them as this one.",
		InputSchema: documentInputSchema(), OutputSchema: documentOutputSchema(),
		Annotations: readOnlyAnnotations(),
	}
}

type fetchDocumentArguments struct {
	Sender   string  `json:"sender,omitempty"`
	Subject  string  `json:"subject,omitempty"`
	Filename string  `json:"filename,omitempty"`
	Date     *string `json:"date,omitempty"`
	Cursor   *string `json:"cursor,omitempty"`
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
	fingerprint := documentLookupFingerprint(mode, args)
	var cursor *documentCursor
	if args.Cursor != nil {
		decoded, err := decodeDocumentCursor(*args.Cursor, fingerprint)
		if err != nil {
			return CallToolResult{}, err
		}
		cursor = &decoded
	}

	var docs []postgres.DocumentLocation
	switch mode {
	case fetchByFilename:
		docs, err = t.Docs.FindByFilename(ctx, strings.TrimSpace(args.Filename))
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
	more := len(docs) > postgres.FetchDocumentMaxResults
	if more {
		docs = docs[:postgres.FetchDocumentMaxResults]
	}

	matches := make([]documentMatch, 0, len(docs))
	for _, d := range docs {
		match, err := t.render(ctx, d)
		if err != nil {
			t.logger().Info("fetch_document", "mode", mode, "error", err.Error())
			return CallToolResult{}, err
		}
		matches = append(matches, match)
	}
	start := documentPosition{phase: phaseText}
	if cursor != nil {
		if start, err = cursor.position(matches); err != nil {
			return CallToolResult{}, err
		}
	}
	result, next, err := buildDocumentPage(matches, more, time.Now().UTC(), start, fingerprint)
	if err != nil {
		t.logger().Info("fetch_document", "mode", mode, "error", err.Error())
		return CallToolResult{}, err
	}
	t.logger().Info("fetch_document", "mode", mode, "matches", len(matches), "more_matches", more,
		"cursor_set", cursor != nil, "complete", next == nil)
	return result, nil
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
// three the tool advertises. A cursor accompanies a lookup; it never replaces
// one.
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
	if args.Cursor != nil && (len(*args.Cursor) == 0 || len(*args.Cursor) > maxCursorBytes) {
		return fetchDocumentNoMode, &argumentError{message: "cursor must be a bounded opaque token returned by this tool"}
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

// FetchedDocument is the part of one matched document a page carries. Its
// identity, location, and attribution are repeated on every page that
// carries any of it; text and attachments are the slices this page delivers.
type FetchedDocument struct {
	DocID               string               `json:"doc_id"`
	ParentDocID         string               `json:"parent_doc_id,omitempty"`
	DocType             string               `json:"doc_type"`
	MimeType            string               `json:"mime_type"`
	SourceFilename      string               `json:"source_filename"`
	Subject             string               `json:"subject,omitempty"`
	From                string               `json:"from,omitempty"`
	To                  []string             `json:"to,omitempty"`
	ToOmitted           int                  `json:"to_omitted,omitempty"`
	Cc                  []string             `json:"cc,omitempty"`
	CcOmitted           int                  `json:"cc_omitted,omitempty"`
	Date                *string              `json:"date,omitempty"`
	SourceLocations     []source.Location    `json:"source_locations,omitempty"`
	SourceRef           string               `json:"source_ref"`
	SourcePath          string               `json:"source_path,omitempty"`
	ContainerSourcePath string               `json:"container_source_path,omitempty"`
	Text                string               `json:"text,omitempty"`
	TextRange           *EvidenceRange       `json:"text_range,omitempty"`
	TextLength          int                  `json:"text_length"`
	TextComplete        bool                 `json:"text_complete"`
	Attachments         []AttachmentDocument `json:"attachments,omitempty"`
	AttachmentsOffset   int                  `json:"attachments_offset"`
	AttachmentCount     int                  `json:"attachment_count"`
}

// AttachmentDocument is a direct child document: identified and located, but
// without its own body text — fetch it by filename for that.
type AttachmentDocument struct {
	DocID               string            `json:"doc_id"`
	DocType             string            `json:"doc_type"`
	MimeType            string            `json:"mime_type"`
	SourceFilename      string            `json:"source_filename"`
	SourceLocations     []source.Location `json:"source_locations,omitempty"`
	SourceRef           string            `json:"source_ref"`
	SourcePath          string            `json:"source_path,omitempty"`
	ContainerSourcePath string            `json:"container_source_path,omitempty"`
}

type DocumentFetchResult struct {
	Documents   []FetchedDocument `json:"documents"`
	MoreMatches bool              `json:"more_matches"`
	Snapshot    time.Time         `json:"snapshot"`
}

// documentMatch is one whole matched document; pages deliver slices of it.
type documentMatch struct {
	header      FetchedDocument
	text        string
	attachments []AttachmentDocument
}

func (t *DocumentTool) render(ctx context.Context, d postgres.DocumentLocation) (documentMatch, error) {
	header := FetchedDocument{
		DocID: d.DocID, ParentDocID: d.ParentDocID, DocType: d.DocType, MimeType: d.MimeType,
		SourceLocations: d.SourceLocations, SourceRef: d.SourceRef, SourceFilename: d.SourceFilename, SourcePath: d.SourcePath, ContainerSourcePath: d.ContainerPath,
	}
	if d.DocType == "email" {
		header.Subject = d.EmailSubject
		header.From = d.EmailFrom
		if d.EmailDate != nil {
			formatted := d.EmailDate.UTC().Format(time.RFC3339)
			header.Date = &formatted
		}
		to, cc, err := t.Docs.EmailRecipients(ctx, d.DocID)
		if err != nil {
			return documentMatch{}, err
		}
		header.To, header.ToOmitted = boundRecipients(to)
		header.Cc, header.CcOmitted = boundRecipients(cc)
	}

	children, err := t.Docs.Children(ctx, d.DocID)
	if err != nil {
		return documentMatch{}, err
	}
	match := documentMatch{header: header, text: d.NormalizedText}
	for _, c := range children {
		match.attachments = append(match.attachments, AttachmentDocument{
			DocID: c.DocID, DocType: c.DocType, MimeType: c.MimeType, SourceFilename: c.SourceFilename,
			SourceLocations: c.SourceLocations, SourceRef: c.SourceRef, SourcePath: c.SourcePath, ContainerSourcePath: c.ContainerPath,
		})
	}
	match.header.TextLength = len(match.text)
	match.header.AttachmentCount = len(match.attachments)
	return match, nil
}

func boundRecipients(addresses []string) ([]string, int) {
	if len(addresses) <= maxDocumentRecipients {
		return addresses, 0
	}
	return addresses[:maxDocumentRecipients], len(addresses) - maxDocumentRecipients
}

// documentPosition is where a page starts: a match, the phase within it, and
// a UTF-8 byte offset into its text or an index into its attachments. A
// match's text is delivered before its attachments.
type documentPosition struct {
	index  int
	phase  string
	offset int
}

const (
	phaseText        = "t"
	phaseAttachments = "a"
)

// buildDocumentPage fills one response page from start, in order, with as
// much of the matches as fits the shared response bounds, and returns the
// position the next page starts at, or nil when this page completes the
// result. Only a single match's identity alone outgrowing a page fails.
func buildDocumentPage(matches []documentMatch, more bool, snapshot time.Time, start documentPosition, fingerprint string) (CallToolResult, *documentPosition, error) {
	result := &DocumentFetchResult{Documents: []FetchedDocument{}, MoreMatches: more, Snapshot: snapshot}
	// Candidates are sized with a maximum-length cursor so the real one,
	// issued once the page is settled, can only make it smaller.
	placeholder := strings.Repeat("x", maxCursorBytes)
	fits := func(entry FetchedDocument) bool {
		candidate := *result
		candidate.Documents = append(append([]FetchedDocument{}, result.Documents...), entry)
		_, err := finalizeDocumentResult(&candidate, &placeholder)
		return err == nil
	}

	pos := start
	var next *documentPosition
	for pos.index < len(matches) {
		match := matches[pos.index]
		entry := match.header
		entry.TextComplete = true
		progressed := false

		if pos.phase == phaseText && pos.offset < len(match.text) {
			end := largestFitting(pos.offset+1, len(match.text), func(end int) bool {
				end = utf8BoundaryAtOrBefore(match.text, end)
				return end > pos.offset && fits(withText(entry, match.text, pos.offset, end))
			})
			if end <= pos.offset {
				if len(result.Documents) > 0 {
					next = &pos
					break
				}
				return CallToolResult{}, nil, &resultSizeError{limit: targetToolResultBytes}
			}
			end = utf8BoundaryAtOrBefore(match.text, end)
			if end < len(match.text) {
				if paragraph := preferredParagraphEnd(match.text, pos.offset, end); paragraph > pos.offset {
					end = paragraph
				}
				result.Documents = append(result.Documents, withText(entry, match.text, pos.offset, end))
				next = &documentPosition{index: pos.index, phase: phaseText, offset: end}
				break
			}
			entry = withText(entry, match.text, pos.offset, end)
			pos = documentPosition{index: pos.index, phase: phaseAttachments}
			progressed = true
		} else if pos.phase == phaseText {
			pos = documentPosition{index: pos.index, phase: phaseAttachments}
		}

		remaining := len(match.attachments) - pos.offset
		count := largestFitting(0, remaining, func(n int) bool {
			return fits(withAttachments(entry, match.attachments, pos.offset, n))
		})
		if count < 0 && progressed {
			// The text already delivered fit with no attachments beside it.
			count = 0
		}
		switch {
		case count == remaining:
			result.Documents = append(result.Documents, withAttachments(entry, match.attachments, pos.offset, count))
			pos = documentPosition{index: pos.index + 1, phase: phaseText}
			continue
		case count > 0 || progressed:
			result.Documents = append(result.Documents, withAttachments(entry, match.attachments, pos.offset, count))
			next = &documentPosition{index: pos.index, phase: phaseAttachments, offset: pos.offset + count}
		case len(result.Documents) > 0:
			next = &pos
		default:
			return CallToolResult{}, nil, &resultSizeError{limit: targetToolResultBytes}
		}
		break
	}

	var cursor *string
	if next != nil {
		encoded, err := encodeDocumentCursor(matches, *next, fingerprint)
		if err != nil {
			return CallToolResult{}, nil, err
		}
		cursor = &encoded
	}
	page, err := finalizeDocumentResult(result, cursor)
	return page, next, err
}

// largestFitting returns the largest value in [low, high] for which fits
// holds, assuming fits is monotone, or low-1 when none does.
func largestFitting(low, high int, fits func(int) bool) int {
	best := low - 1
	for low <= high {
		middle := low + (high-low)/2
		if fits(middle) {
			best = middle
			low = middle + 1
		} else {
			high = middle - 1
		}
	}
	return best
}

func withText(entry FetchedDocument, text string, start, end int) FetchedDocument {
	entry.Text = text[start:end]
	entry.TextRange = &EvidenceRange{Start: start, End: end, Unit: utf8ByteUnit}
	entry.TextComplete = end == len(text)
	return entry
}

func withAttachments(entry FetchedDocument, attachments []AttachmentDocument, offset, count int) FetchedDocument {
	entry.Attachments = attachments[offset : offset+count]
	entry.AttachmentsOffset = offset
	return entry
}

// documentCursor is a stateless continuation: it names the lookup it belongs
// to, the match and position to resume at, and the revision of that match's
// text, so a continuation re-runs the lookup rather than holding any state
// here. Field names are short because the encoded form is size-bounded.
type documentCursor struct {
	Version     int    `json:"v"`
	Fingerprint string `json:"f"`
	DocID       string `json:"d"`
	TextHash    string `json:"h"`
	Phase       string `json:"p"`
	Offset      int    `json:"o"`
}

var errStaleDocumentCursor = &argumentError{message: "cursor no longer matches this document; fetch it again without a cursor"}

func encodeDocumentCursor(matches []documentMatch, pos documentPosition, fingerprint string) (string, error) {
	match := matches[pos.index]
	raw, err := json.Marshal(documentCursor{
		Version: documentCursorVersion, Fingerprint: fingerprint, DocID: match.header.DocID,
		TextHash: documentTextHash(match.text), Phase: pos.phase, Offset: pos.offset,
	})
	if err != nil {
		return "", fmt.Errorf("encode document cursor: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	if len(encoded) > maxCursorBytes {
		return "", fmt.Errorf("document cursor exceeds %d bytes", maxCursorBytes)
	}
	return encoded, nil
}

func decodeDocumentCursor(value, fingerprint string) (documentCursor, error) {
	invalid := &argumentError{message: "cursor is invalid for this fetch; pass next_cursor unchanged with the same arguments"}
	var c documentCursor
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || json.Unmarshal(raw, &c) != nil {
		return c, invalid
	}
	if c.Version != documentCursorVersion || c.Fingerprint != fingerprint || c.DocID == "" || c.Offset < 0 ||
		(c.Phase != phaseText && c.Phase != phaseAttachments) {
		return c, invalid
	}
	return c, nil
}

// position resolves the cursor against a fresh run of its lookup. A match
// that is gone, or whose text changed, cannot be resumed without mixing two
// revisions, so the caller starts again.
func (c documentCursor) position(matches []documentMatch) (documentPosition, error) {
	for i, match := range matches {
		if match.header.DocID != c.DocID {
			continue
		}
		if documentTextHash(match.text) != c.TextHash {
			return documentPosition{}, errStaleDocumentCursor
		}
		switch {
		case c.Phase == phaseText && (c.Offset >= len(match.text) || !utf8.RuneStart(match.text[c.Offset])):
			return documentPosition{}, errStaleDocumentCursor
		case c.Phase == phaseAttachments && c.Offset > len(match.attachments):
			return documentPosition{}, errStaleDocumentCursor
		}
		return documentPosition{index: i, phase: c.Phase, offset: c.Offset}, nil
	}
	return documentPosition{}, errStaleDocumentCursor
}

func documentTextHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:8])
}

// documentLookupFingerprint identifies the lookup a cursor was issued for,
// so a cursor presented with different arguments is refused rather than
// resumed against another result.
func documentLookupFingerprint(mode fetchDocumentMode, args fetchDocumentArguments) string {
	date := ""
	if args.Date != nil {
		if day, err := parseMailboxDate(args.Date); err == nil {
			date = truncateToDay(day).Format(time.DateOnly)
		}
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		string(mode), strings.TrimSpace(args.Filename), normalizeExactAddress(args.Sender), strings.TrimSpace(args.Subject), date,
	}, "\x00")))
	return hex.EncodeToString(sum[:8])
}

type documentResult struct {
	Kind             string         `json:"kind"`
	Result           any            `json:"result"`
	Complete         bool           `json:"complete"`
	NextCursor       *string        `json:"next_cursor"`
	ContinuationTool *string        `json:"continuation_tool"`
	ResponseBudget   ResponseBudget `json:"response_budget"`
}

// finalizeDocumentResult applies the same budget/size/line bounds as
// finalizeMailboxResult (internal/mcp/mailbox.go): a fixed-point pass to
// measure the response's own encoded size accurately, then the shared MCP
// response ceilings. A nil cursor marks the page complete.
func finalizeDocumentResult(value *DocumentFetchResult, cursor *string) (CallToolResult, error) {
	page := &documentResult{Kind: "document_fetch", Result: value, Complete: cursor == nil, NextCursor: cursor,
		ResponseBudget: ResponseBudget{Allowed: targetToolResultBytes, Unit: utf8ByteUnit}}
	if cursor != nil {
		tool := "fetch_document"
		page.ContinuationTool = &tool
	}
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
	fmt.Fprintf(&b, "Document fetch: %d match(es) on this page.\n", len(result.Documents))
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
		writeRecipients(&b, "to", d.To, d.ToOmitted)
		writeRecipients(&b, "cc", d.Cc, d.CcOmitted)
		if d.TextRange != nil {
			fmt.Fprintf(&b, "  text, bytes %d-%d of %d:\n%s\n", d.TextRange.Start, d.TextRange.End, d.TextLength, d.Text)
			if !d.TextComplete {
				b.WriteString("  text continues on the next page\n")
			}
		}
		if len(d.Attachments) > 0 {
			fmt.Fprintf(&b, "  attachments %d-%d of %d:\n", d.AttachmentsOffset+1, d.AttachmentsOffset+len(d.Attachments), d.AttachmentCount)
			for _, a := range d.Attachments {
				fmt.Fprintf(&b, "    - %s", a.SourceFilename)
				switch {
				case a.SourcePath != "":
					fmt.Fprintf(&b, " (%s)", a.SourcePath)
				case a.ContainerSourcePath != "":
					fmt.Fprintf(&b, " (extracted from %s)", a.ContainerSourcePath)
				}
				b.WriteString("\n")
			}
		}
	}
	if result.MoreMatches {
		fmt.Fprintf(&b, "More than %d documents match; only the first %d are returned. Narrow with a more exact subject or date.\n", postgres.FetchDocumentMaxResults, postgres.FetchDocumentMaxResults)
	}
	if page.NextCursor != nil {
		fmt.Fprintf(&b, "This result continues. Call fetch_document again with the same arguments plus exactly {\"cursor\":%q}.\n", *page.NextCursor)
	}
	fmt.Fprintf(&b, "Snapshot: %s. Response budget: %d of %d UTF-8 bytes.\n", result.Snapshot.Format(time.RFC3339), page.ResponseBudget.Used, page.ResponseBudget.Allowed)
	return b.String()
}

func writeRecipients(b *strings.Builder, label string, addresses []string, omitted int) {
	if len(addresses) == 0 {
		return
	}
	fmt.Fprintf(b, "  %s: %s", label, strings.Join(addresses, ", "))
	if omitted > 0 {
		fmt.Fprintf(b, " and %d more", omitted)
	}
	b.WriteString("\n")
}

func documentInputSchema() map[string]any {
	return map[string]any{
		"$schema": jsonSchema202012, "type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"sender":   map[string]any{"type": "string", "maxLength": maxMailboxAddressRunes, "description": "One exact normalized sender mailbox. Requires date."},
			"subject":  map[string]any{"type": "string", "maxLength": maxDocumentSubjectRunes, "description": "One exact email subject. Requires date."},
			"filename": map[string]any{"type": "string", "maxLength": maxDocumentFilenameRunes, "description": "One exact original document filename. Not combined with date."},
			"date":     nullableDateSchema(),
			"cursor":   map[string]any{"type": "string", "minLength": 1, "maxLength": maxCursorBytes, "description": "Opaque next_cursor from the previous page of this same lookup. Pass it unchanged, with the same arguments."},
		},
	}
}

func documentOutputSchema() map[string]any {
	stringArray := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	count := map[string]any{"type": "integer", "minimum": 0}
	textRange := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"start": count, "end": count, "unit": map[string]any{"const": utf8ByteUnit},
	}, "required": []string{"start", "end", "unit"}}
	attachment := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"doc_id": map[string]any{"type": "string", "minLength": 1}, "doc_type": map[string]any{"type": "string"}, "mime_type": map[string]any{"type": "string"},
		"source_filename": map[string]any{"type": "string"}, "source_ref": map[string]any{"type": "string"}, "source_locations": map[string]any{"type": "array", "items": map[string]any{"type": "object"}}, "source_path": map[string]any{"type": "string"}, "container_source_path": map[string]any{"type": "string"},
	}, "required": []string{"doc_id", "doc_type", "mime_type", "source_filename"}}
	document := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"doc_id": map[string]any{"type": "string", "minLength": 1}, "parent_doc_id": map[string]any{"type": "string"},
		"doc_type": map[string]any{"type": "string"}, "mime_type": map[string]any{"type": "string"}, "source_filename": map[string]any{"type": "string"},
		"subject": map[string]any{"type": "string"}, "from": map[string]any{"type": "string"}, "to": stringArray, "to_omitted": count, "cc": stringArray, "cc_omitted": count,
		"date": map[string]any{"type": "string", "format": "date-time"}, "source_ref": map[string]any{"type": "string"}, "source_locations": map[string]any{"type": "array", "items": map[string]any{"type": "object"}}, "source_path": map[string]any{"type": "string"}, "container_source_path": map[string]any{"type": "string"},
		"text": map[string]any{"type": "string"}, "text_range": textRange, "text_length": count, "text_complete": map[string]any{"type": "boolean"},
		"attachments": map[string]any{"type": "array", "items": attachment}, "attachments_offset": count, "attachment_count": count,
	}, "required": []string{"doc_id", "doc_type", "mime_type", "source_filename", "text_length", "text_complete", "attachments_offset", "attachment_count"}}
	nullableString := map[string]any{"type": []string{"string", "null"}}
	return map[string]any{
		"$schema": jsonSchema202012, "type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"kind": map[string]any{"const": "document_fetch"},
			"result": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
				"documents": map[string]any{"type": "array", "items": document}, "more_matches": map[string]any{"type": "boolean"}, "snapshot": map[string]any{"type": "string", "format": "date-time"},
			}, "required": []string{"documents", "more_matches", "snapshot"}},
			"complete": map[string]any{"type": "boolean"}, "next_cursor": nullableString, "continuation_tool": nullableString,
			"response_budget": map[string]any{"$ref": "#/$defs/budget"},
		},
		"required": []string{"kind", "result", "complete", "next_cursor", "continuation_tool", "response_budget"},
		"$defs":    map[string]any{"budget": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"used": map[string]any{"type": "integer", "minimum": 1}, "allowed": map[string]any{"const": targetToolResultBytes}, "unit": map[string]any{"const": utf8ByteUnit}}, "required": []string{"used", "allowed", "unit"}}},
	}
}
