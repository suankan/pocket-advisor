package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/suankan/pocket-advisor/internal/retrieval"
)

const (
	defaultTopK      = 15
	maxTopK          = 50
	maxQuestionRunes = 8192
	maxCursorBytes   = 256
)

// Retriever is the transport-independent query boundary used by the MCP
// adapter. retrieval.Service satisfies it; the exported interface also permits
// synthetic client fixtures without constructing storage or model clients.
type Retriever interface {
	Query(context.Context, retrieval.Request) (*retrieval.Result, error)
}

// QueryTool exposes retrieval.Query. The workspace is fixed at startup rather
// than passed per call. Each workspace has its own database and role, and the
// retrieval service asserts that scope before this tool is served.
type QueryTool struct {
	Service   Retriever
	Workspace string

	// Title and Corpus describe what the selected workspace holds. They come
	// from the private registry at runtime; committed tests use synthetic names.
	Title  string
	Corpus []string
	// Mailbox, when configured, exposes deterministic email browse tools in
	// the same fixed workspace as retrieval.
	Mailbox *MailboxTool
	// Timeline, when configured, follows source-backed topic mentions from
	// the active graph in this same fixed workspace.
	Timeline *TimelineTool

	stateMu          sync.Mutex
	store            *snapshotStore
	now              func() time.Time
	random           io.Reader
	snapshotTTL      time.Duration
	maxSnapshots     int
	maxSnapshotBytes int
}

// forCaller returns a tool with the same immutable retrieval dependencies and
// presentation metadata, but an independent continuation namespace. HTTP uses
// one such tool per authenticated subject so a cursor can survive token
// renewal without becoming usable by another caller. Stdio already creates
// one QueryTool per process and therefore does not need this clone.
func (t *QueryTool) forCaller() *QueryTool {
	return &QueryTool{
		Service:          t.Service,
		Workspace:        t.Workspace,
		Title:            t.Title,
		Corpus:           append([]string(nil), t.Corpus...),
		Mailbox:          t.Mailbox,
		Timeline:         t.Timeline,
		now:              t.now,
		random:           t.random,
		snapshotTTL:      t.snapshotTTL,
		maxSnapshots:     t.maxSnapshots,
		maxSnapshotBytes: t.maxSnapshotBytes,
	}
}

type ToolDefinition struct {
	Name         string          `json:"name"`
	Title        string          `json:"title"`
	Description  string          `json:"description"`
	InputSchema  map[string]any  `json:"inputSchema"`
	OutputSchema map[string]any  `json:"outputSchema"`
	Annotations  ToolAnnotations `json:"annotations"`
}

type ToolAnnotations struct {
	ReadOnlyHint    bool `json:"readOnlyHint"`
	DestructiveHint bool `json:"destructiveHint"`
	IdempotentHint  bool `json:"idempotentHint"`
	OpenWorldHint   bool `json:"openWorldHint"`
}

func (t *QueryTool) normalizedWorkspace() string {
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

// Name is transport-stable. The process has one fixed workspace, so a tool
// name need not disclose or select that workspace.
func (t *QueryTool) Name() string { return "search" }

// ReadName is the companion tool for following opaque continuation cursors.
func (t *QueryTool) ReadName() string { return "read_evidence" }

func readOnlyAnnotations() ToolAnnotations {
	return ToolAnnotations{
		ReadOnlyHint: true, DestructiveHint: false, IdempotentHint: true, OpenWorldHint: false,
	}
}

// Describe returns the compact search tool definition. Its schemas are closed
// and use JSON Schema 2020-12, the default dialect in MCP 2025-11-25.
func (t *QueryTool) Describe() ToolDefinition {
	title := t.Title
	if title == "" {
		title = t.Workspace
	}
	contents := ""
	if len(t.Corpus) > 0 {
		contents = "\n\nThis corpus contains:\n  - " + strings.Join(t.Corpus, "\n  - ") + "\n"
	}

	return ToolDefinition{
		Name:  t.Name(),
		Title: "Search " + title + " evidence",
		Description: "Search " + title + " — an ingested corpus of this user's own " +
			"documents — and return a compact ranked evidence index with result-scoped references." +
			contents + "\n" +
			"Use this only for questions about what these documents say. Other servers " +
			"may hold different corpora; choose by the contents listed above rather than by guessing.\n\n" +
			"This tool returns evidence, not an answer. Cite the complete references shown, " +
			"such as [R0123456789ab:E1]. When complete=false, call the named continuation_tool " +
			"with exactly next_cursor and continue until complete=true before claiming complete " +
			"coverage. Never invent a cursor, byte range, document identifier, or workspace.\n\n" +
			"Every document reports where to find it, relative to the workspace's local staging " +
			"directory on the operator's machine. document.workspace_relative_path is the " +
			"document's own file. When that is null, document.container_workspace_relative_path " +
			"is the file it was extracted from — say it has no file of its own and name that " +
			"container, such as the email holding an attachment. Exactly one of the two is " +
			"always present. Report the path the tool gave you and do not try to open, read, or " +
			"fetch it yourself. Never infer a location from where similar or sibling documents " +
			"live; a plausible guess is frequently wrong.\n\n" +
			"If packets is empty and complete=true, say that the corpus supplied no supporting " +
			"evidence rather than answering from general knowledge. The corpus may contain " +
			"English and Russian; ask in either language.",
		InputSchema: queryInputSchema(), OutputSchema: evidencePageOutputSchema(),
		Annotations: readOnlyAnnotations(),
	}
}

// DescribeRead returns the cursor-only evidence reader. The cursor is opaque,
// session-local, and fixes both workspace and immutable retrieval snapshot.
func (t *QueryTool) DescribeRead() ToolDefinition {
	title := t.Title
	if title == "" {
		title = t.Workspace
	}
	return ToolDefinition{
		Name:  t.ReadName(),
		Title: "Read " + title + " evidence",
		Description: "Read the next bounded page from an earlier search result. Pass exactly " +
			"the opaque next_cursor returned by search or by this tool. Do not construct a cursor " +
			"or request a byte range, document, result, or workspace directly. Cite the complete " +
			"result-scoped references on the page. If complete=false, call this tool again with " +
			"next_cursor; only complete=true means all evidence admitted by that search result's " +
			"aggregate evidence budget has been delivered.",
		InputSchema: cursorInputSchema(), OutputSchema: evidencePageOutputSchema(),
		Annotations: readOnlyAnnotations(),
	}
}

func (t *QueryTool) DescribeAll() []ToolDefinition {
	definitions := []ToolDefinition{t.Describe(), t.DescribeRead()}
	if t.Mailbox != nil {
		definitions = append(definitions, t.Mailbox.DescribeAll()...)
	}
	if t.Timeline != nil {
		definitions = append(definitions, t.Timeline.Describe())
	}
	return definitions
}

type rawCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Meta      json.RawMessage `json:"_meta,omitempty"`
	Task      json.RawMessage `json:"task,omitempty"`
}

type queryArguments struct {
	Question string `json:"question"`
	TopK     *int   `json:"top_k,omitempty"`
}

type cursorArguments struct {
	Cursor string `json:"cursor"`
}

// Call validates a tools/call request and dispatches either a fresh retrieval
// or a read from the immutable session-local snapshot created by that search.
func (t *QueryTool) Call(ctx context.Context, raw json.RawMessage) (CallToolResult, error) {
	var params rawCallParams
	if err := decodeStrict(raw, &params); err != nil {
		return CallToolResult{}, &argumentError{message: "tools/call params must be a valid object"}
	}
	if params.Name == "" {
		return CallToolResult{}, &argumentError{message: "tool name is required"}
	}
	if t.Mailbox != nil && (params.Name == t.Mailbox.ListName() || params.Name == t.Mailbox.ConversationName() || params.Name == t.Mailbox.AwaitingReplyName()) {
		return t.Mailbox.Call(ctx, raw)
	}
	if t.Timeline != nil && params.Name == t.Timeline.Name() {
		return t.Timeline.Call(ctx, raw)
	}
	if params.Name != t.Name() && params.Name != t.ReadName() {
		return CallToolResult{}, &unknownToolError{}
	}
	if len(params.Task) > 0 && string(params.Task) != "null" {
		return CallToolResult{}, &argumentError{message: "task-augmented execution is not supported"}
	}

	if params.Name == t.ReadName() {
		var args cursorArguments
		if err := decodeStrict(params.Arguments, &args); err != nil {
			return CallToolResult{}, &argumentError{message: "arguments must match the advertised cursor-only input schema"}
		}
		if args.Cursor == "" || len(args.Cursor) > maxCursorBytes {
			return CallToolResult{}, &argumentError{message: "cursor must be a non-empty opaque token returned by this session"}
		}
		return t.readEvidencePage(ctx, args.Cursor)
	}

	var args queryArguments
	if err := decodeStrict(params.Arguments, &args); err != nil {
		return CallToolResult{}, &argumentError{message: "arguments must match the advertised input schema"}
	}
	if utf8.RuneCountInString(args.Question) > maxQuestionRunes {
		return CallToolResult{}, &argumentError{message: fmt.Sprintf("question must not exceed %d Unicode characters", maxQuestionRunes)}
	}
	question := strings.TrimSpace(args.Question)
	if question == "" {
		return CallToolResult{}, &argumentError{message: "question is required"}
	}
	topK := 0
	if args.TopK != nil {
		if *args.TopK < 1 || *args.TopK > maxTopK {
			return CallToolResult{}, &argumentError{message: fmt.Sprintf("top_k must be between 1 and %d", maxTopK)}
		}
		topK = *args.TopK
	}

	if t.Service == nil {
		return CallToolResult{}, fmt.Errorf("retrieval service is unavailable")
	}
	res, err := t.Service.Query(ctx, retrieval.Request{Question: question, TopK: topK})
	if err != nil {
		return CallToolResult{}, err
	}
	resultID, err := t.newResultID()
	if err != nil {
		return CallToolResult{}, err
	}
	evidence, err := newEvidenceResult(res, resultID)
	if err != nil {
		t.releaseResultID(resultID)
		return CallToolResult{}, fmt.Errorf("build evidence result: %w", err)
	}
	return t.storeSearchResult(evidence)
}

type argumentError struct{ message string }

func (e *argumentError) Error() string { return e.message }

type unknownToolError struct{}

func (*unknownToolError) Error() string { return "unknown tool" }

type resultSizeError struct{ limit int }

func (e *resultSizeError) Error() string { return fmt.Sprintf("tool result exceeds %d bytes", e.limit) }

type resultLineError struct{ limit int }

func (e *resultLineError) Error() string {
	return fmt.Sprintf("tool result exceeds %d readable lines", e.limit)
}

func errorResult(err error) CallToolResult {
	message := "Evidence search is temporarily unavailable. Do not answer from general knowledge; report that retrieval failed."
	var args *argumentError
	var size *resultSizeError
	var lines *resultLineError
	switch {
	case errors.As(err, &args):
		message = "Invalid arguments: " + args.message + ". Correct the request and retry."
	case errors.As(err, &size):
		message = fmt.Sprintf("An evidence page could not fit the %d-byte safe response limit. Narrow the question and run the search again.", size.limit)
	case errors.As(err, &lines):
		message = fmt.Sprintf("An evidence page could not fit the %d-line safe response limit. Narrow the question and run the search again.", lines.limit)
	}
	return CallToolResult{Content: []TextContent{{Type: "text", Text: message}}, IsError: true}
}

func decodeStrict(raw json.RawMessage, target any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return fmt.Errorf("missing JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("trailing JSON value")
	}
	return nil
}
