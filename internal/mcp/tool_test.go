package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/suankan/pocket-advisor/internal/retrieval"
)

type stubRetriever struct {
	mu     sync.Mutex
	result *retrieval.Result
	err    error
	got    retrieval.Request
}

func (s *stubRetriever) Query(_ context.Context, req retrieval.Request) (*retrieval.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = req
	return s.result, s.err
}

func syntheticResult() *retrieval.Result {
	return syntheticTextResult("Привет from synthetic evidence")
}

func syntheticTextResult(text string) *retrieval.Result {
	date := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	return &retrieval.Result{
		Question: "What does the synthetic evidence say?", SubQueries: []string{"synthetic evidence", "Привет"},
		Warnings: []string{retrieval.WarnBudgetTruncated}, Budget: retrieval.Budget{BytesUsed: len(text), BytesAllowed: len(text)},
		Packets: []retrieval.Packet{{
			Document: retrieval.Document{
				DocID: "11111111-1111-1111-1111-111111111111", ThreadID: "thread-1", DocType: "email",
				Title: "Synthetic evidence", From: "sender@example.test", To: "reader@example.test", Date: &date,
				RawURI: "s3://synthetic/raw/abc", SHA256: strings.Repeat("a", 64), CharCount: len(text),
			},
			Match: retrieval.Match{
				ChunkID: "22222222-2222-2222-2222-222222222222", StartByte: 0, EndByte: len("Привет"),
				Score: 0.75, Legs: "both", SubQuery: "Привет", Snippet: "Привет",
			},
			Text: text,
			Related: []retrieval.Related{{
				Document: retrieval.Document{
					DocID: "33333333-3333-3333-3333-333333333333", DocType: "email", Title: "Synthetic parent",
					RawURI: "s3://synthetic/raw/def", SHA256: strings.Repeat("b", 64), CharCount: 20,
				},
				Relation: retrieval.RelationParent,
			}},
		}},
	}
}

func searchCall(t *testing.T, tool *QueryTool, question string) CallToolResult {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"name": tool.Name(), "arguments": map[string]any{"question": question},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.Call(context.Background(), raw)
	if err != nil {
		t.Fatalf("search call: %v", err)
	}
	return result
}

func readCall(t *testing.T, tool *QueryTool, cursor string) CallToolResult {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"name": tool.ReadName(), "arguments": map[string]any{"cursor": cursor},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.Call(context.Background(), raw)
	if err != nil {
		t.Fatalf("read call: %v", err)
	}
	return result
}

func resultPage(t *testing.T, result CallToolResult) *EvidencePage {
	t.Helper()
	page, ok := result.StructuredContent.(*EvidencePage)
	if !ok {
		t.Fatalf("structured content type = %T", result.StructuredContent)
	}
	return page
}

func assertBoundedPage(t *testing.T, result CallToolResult) {
	t.Helper()
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > targetToolResultBytes {
		t.Errorf("encoded result = %d bytes, target = %d", len(encoded), targetToolResultBytes)
	}
	if lines := readableLines(result.Content[0].Text); lines > targetReadableLines {
		t.Errorf("readable result = %d lines, target = %d", lines, targetReadableLines)
	}
	validateAgainstOutputSchema(t, resultPage(t, result))
}

func TestSuccessfulSearchReturnsCompactSchemaValidIndex(t *testing.T) {
	stub := &stubRetriever{result: syntheticResult()}
	tool := &QueryTool{Service: stub, Workspace: "synthetic"}
	result, err := tool.Call(context.Background(), json.RawMessage(
		`{"name":"search_synthetic","arguments":{"question":"  What does the synthetic evidence say?  ","top_k":3}}`,
	))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %+v", result)
	}
	if stub.got.Question != "What does the synthetic evidence say?" || stub.got.TopK != 3 {
		t.Errorf("retrieval request = %+v", stub.got)
	}

	page := resultPage(t, result)
	if page.Kind != "search" || page.Complete || page.NextCursor == nil || page.ContinuationTool == nil {
		t.Fatalf("search page continuation = %+v", page)
	}
	if page.EvidenceBudget.Unit != utf8ByteUnit || len(page.Packets) != 1 || len(page.Segments) != 0 {
		t.Fatalf("search page shape = %+v", page)
	}
	packet := page.Packets[0]
	if packet.Reference != page.ResultID+":E1" || packet.Rank != 1 {
		t.Errorf("packet identity = %q rank %d", packet.Reference, packet.Rank)
	}
	if packet.Match.End != len("Привет") || packet.Match.OffsetUnit != utf8ByteUnit {
		t.Errorf("match offsets = %+v", packet.Match)
	}
	if !packet.TextAvailable || packet.RelatedTextAvailable != 0 {
		t.Errorf("availability = %+v", packet)
	}
	fallback := result.Content[0].Text
	for _, want := range []string{"[" + packet.Reference + "]", "UTF-8 bytes 0-12", "MORE ADMITTED EVIDENCE", tool.ReadName()} {
		if !strings.Contains(fallback, want) {
			t.Errorf("text fallback should contain %q", want)
		}
	}
	if strings.Contains(fallback, "Привет from synthetic evidence") {
		t.Error("compact search page must not embed admitted full text")
	}
	assertBoundedPage(t, result)
}

func TestPublishedSchemasAreClosedAndBounded(t *testing.T) {
	query := compileJSONSchema(t, "query.schema.json", queryInputSchema())
	if err := query.Validate(map[string]any{"question": "synthetic", "top_k": 3}); err != nil {
		t.Fatalf("valid query rejected: %v", err)
	}
	for name, invalid := range map[string]any{
		"missing question": map[string]any{},
		"zero top_k":       map[string]any{"question": "synthetic", "top_k": 0},
		"workspace":        map[string]any{"question": "synthetic", "workspace": "other"},
	} {
		if err := query.Validate(invalid); err == nil {
			t.Errorf("%s unexpectedly passed", name)
		}
	}
	cursor := compileJSONSchema(t, "cursor.schema.json", cursorInputSchema())
	if err := cursor.Validate(map[string]any{"cursor": "opaque"}); err != nil {
		t.Fatalf("valid cursor rejected: %v", err)
	}
	for name, invalid := range map[string]any{
		"range":     map[string]any{"cursor": "opaque", "start": 10},
		"workspace": map[string]any{"cursor": "opaque", "workspace": "other"},
		"result":    map[string]any{"result_id": "R000000000000"},
	} {
		if err := cursor.Validate(invalid); err == nil {
			t.Errorf("%s unexpectedly passed", name)
		}
	}
}

func TestRuntimeQuestionBoundMatchesSchemaBeforeTrimming(t *testing.T) {
	stub := &stubRetriever{result: syntheticResult()}
	tool := &QueryTool{Service: stub, Workspace: "synthetic"}
	tooLong := "synthetic" + strings.Repeat(" ", maxQuestionRunes)
	raw, _ := json.Marshal(map[string]any{"name": tool.Name(), "arguments": map[string]any{"question": tooLong}})
	_, err := tool.Call(context.Background(), raw)
	var invalid *argumentError
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %T %v, want argumentError", err, err)
	}
	if stub.got.Question != "" {
		t.Errorf("retrieval unexpectedly ran: %+v", stub.got)
	}
}

func TestEmptyEvidenceIsCompleteAndUsesStableEmptyArrays(t *testing.T) {
	tool := &QueryTool{Service: &stubRetriever{result: &retrieval.Result{
		Question: "No synthetic match", SubQueries: []string{"No synthetic match"}, Packets: []retrieval.Packet{}, Warnings: []string{},
		Budget: retrieval.Budget{BytesAllowed: 1024},
	}}, Workspace: "synthetic"}
	result := searchCall(t, tool, "No synthetic match")
	page := resultPage(t, result)
	if !page.Complete || page.NextCursor != nil || page.Packets == nil || page.Segments == nil || page.Warnings == nil || page.SubQueries == nil {
		t.Fatalf("empty page = %+v", page)
	}
	if !strings.Contains(result.Content[0].Text, "general knowledge") {
		t.Error("empty result must instruct the client not to invent an answer")
	}
	assertBoundedPage(t, result)
}

func TestLargeUTF8DocumentPaginatesWithoutLoss(t *testing.T) {
	text := strings.Repeat("paragraph αβγ🙂 with synthetic evidence\n\n", 3500)
	tool := &QueryTool{Service: &stubRetriever{result: syntheticTextResult(text)}, Workspace: "synthetic"}
	result := searchCall(t, tool, "large synthetic document")
	page := resultPage(t, result)
	reference := page.Packets[0].Reference
	var reconstructed strings.Builder
	lastEnd := 0
	pages := 1
	assertBoundedPage(t, result)
	for !page.Complete {
		result = readCall(t, tool, *page.NextCursor)
		assertBoundedPage(t, result)
		page = resultPage(t, result)
		pages++
		for _, segment := range page.Segments {
			if segment.Reference != reference || segment.Range.Start != lastEnd || !utf8.ValidString(segment.Text) {
				t.Fatalf("invalid segment boundary: %+v, last end %d", segment, lastEnd)
			}
			reconstructed.WriteString(segment.Text)
			lastEnd = segment.Range.End
		}
		if pages > 100 {
			t.Fatal("continuation did not terminate")
		}
	}
	if pages < 4 {
		t.Errorf("large document used only %d pages", pages)
	}
	if reconstructed.String() != text || lastEnd != len(text) {
		t.Fatalf("reconstructed %d of %d bytes", reconstructed.Len(), len(text))
	}
}

func TestReadableLineTargetPaginatesManyShortLines(t *testing.T) {
	text := strings.Repeat("x\n", targetReadableLines+500)
	tool := &QueryTool{Service: &stubRetriever{result: syntheticTextResult(text)}, Workspace: "synthetic"}
	page := resultPage(t, searchCall(t, tool, "many lines"))
	result := readCall(t, tool, *page.NextCursor)
	page = resultPage(t, result)
	if page.Complete {
		t.Fatal("line-bound page unexpectedly completed the document")
	}
	if got := readableLines(result.Content[0].Text); got > targetReadableLines || got < targetReadableLines-20 {
		t.Errorf("readable lines = %d", got)
	}
	assertBoundedPage(t, result)
}

func TestMultiplePacketsKeepReferencesAcrossPages(t *testing.T) {
	base := syntheticResult()
	base.Packets = nil
	base.Budget = retrieval.Budget{}
	for i := range 4 {
		packet := syntheticTextResult(strings.Repeat(fmt.Sprintf("packet-%d 🙂 ", i+1), 1200)).Packets[0]
		packet.DocID = fmt.Sprintf("11111111-1111-1111-1111-%012d", i+1)
		packet.Match.ChunkID = fmt.Sprintf("22222222-2222-2222-2222-%012d", i+1)
		packet.RawURI = fmt.Sprintf("s3://synthetic/raw/%d", i+1)
		packet.SHA256 = strings.Repeat(fmt.Sprintf("%x", i+1), 64)
		packet.Related = []retrieval.Related{}
		base.Packets = append(base.Packets, packet)
		base.Budget.BytesUsed += len(packet.Text)
	}
	base.Budget.BytesAllowed = base.Budget.BytesUsed
	tool := &QueryTool{Service: &stubRetriever{result: base}, Workspace: "synthetic"}
	page := resultPage(t, searchCall(t, tool, "multiple packets"))
	want := make(map[string]string)
	for _, packet := range page.Packets {
		want[packet.Reference] = base.Packets[packet.Rank-1].Text
	}
	got := make(map[string]string)
	for !page.Complete {
		result := readCall(t, tool, *page.NextCursor)
		assertBoundedPage(t, result)
		page = resultPage(t, result)
		for _, segment := range page.Segments {
			got[segment.Reference] += segment.Text
		}
	}
	if len(got) != len(want) {
		t.Fatalf("delivered %d packet texts, want %d", len(got), len(want))
	}
	for reference, text := range want {
		if got[reference] != text {
			t.Errorf("packet %s text did not survive pagination", reference)
		}
	}
}

func TestCompactIndexPaginatesManyPackets(t *testing.T) {
	base := syntheticResult()
	base.Packets = nil
	base.Budget = retrieval.Budget{BytesAllowed: 1}
	for i := range maxTopK {
		packet := syntheticResult().Packets[0]
		packet.DocID = fmt.Sprintf("11111111-1111-1111-1111-%012d", i+1)
		packet.Match.ChunkID = fmt.Sprintf("22222222-2222-2222-2222-%012d", i+1)
		packet.Title = fmt.Sprintf("Synthetic evidence packet %02d with bounded metadata", i+1)
		packet.RawURI = fmt.Sprintf("s3://synthetic/raw/index-%02d", i+1)
		packet.SHA256 = strings.Repeat(fmt.Sprintf("%x", i%15+1), 64)
		packet.Match.Snippet = strings.Repeat("bounded ", 30)
		packet.Text = ""
		packet.CharCount = 0
		packet.Related = []retrieval.Related{}
		base.Packets = append(base.Packets, packet)
	}
	tool := &QueryTool{Service: &stubRetriever{result: base}, Workspace: "synthetic"}
	result := searchCall(t, tool, "many indexed packets")
	page := resultPage(t, result)
	pages := 1
	packets := len(page.Packets)
	assertBoundedPage(t, result)
	for !page.Complete {
		result = readCall(t, tool, *page.NextCursor)
		assertBoundedPage(t, result)
		page = resultPage(t, result)
		if page.Kind != "index" {
			t.Fatalf("page kind = %q, want index", page.Kind)
		}
		packets += len(page.Packets)
		pages++
	}
	if pages < 2 || packets != maxTopK {
		t.Fatalf("index used %d pages and delivered %d packets, want multiple pages and %d packets", pages, packets, maxTopK)
	}
}

func TestContinuationRetryIsIdempotentIncludingTerminalPage(t *testing.T) {
	tool := &QueryTool{Service: &stubRetriever{result: syntheticResult()}, Workspace: "synthetic"}
	search := resultPage(t, searchCall(t, tool, "retry"))
	first := readCall(t, tool, *search.NextCursor)
	second := readCall(t, tool, *search.NextCursor)
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("cursor retry changed page:\n%s\n%s", firstJSON, secondJSON)
	}
	page := resultPage(t, first)
	for !page.Complete {
		first = readCall(t, tool, *page.NextCursor)
		page = resultPage(t, first)
	}
	if page.NextCursor != nil {
		t.Fatal("terminal page has a cursor")
	}
}

func TestContinuationExpiryEvictionAndSessionBinding(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tool := &QueryTool{Service: &stubRetriever{result: syntheticResult()}, Workspace: "one", now: func() time.Time { return now }, snapshotTTL: time.Minute}
	first := resultPage(t, searchCall(t, tool, "first"))
	cursor := *first.NextCursor
	now = now.Add(2 * time.Minute)
	assertInvalidCursor(t, tool, cursor)

	now = now.Add(time.Minute)
	evicting := &QueryTool{Service: &stubRetriever{result: syntheticResult()}, Workspace: "one", now: func() time.Time { return now }, maxSnapshots: 1}
	old := resultPage(t, searchCall(t, evicting, "old"))
	_ = searchCall(t, evicting, "new")
	assertInvalidCursor(t, evicting, *old.NextCursor)

	source := &QueryTool{Service: &stubRetriever{result: syntheticResult()}, Workspace: "one"}
	sourcePage := resultPage(t, searchCall(t, source, "session one"))
	otherSession := &QueryTool{Service: &stubRetriever{result: syntheticResult()}, Workspace: "one"}
	otherWorkspace := &QueryTool{Service: &stubRetriever{result: syntheticResult()}, Workspace: "two"}
	assertInvalidCursor(t, otherSession, *sourcePage.NextCursor)
	assertInvalidCursor(t, otherWorkspace, *sourcePage.NextCursor)
}

func assertInvalidCursor(t *testing.T, tool *QueryTool, cursor string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"name": tool.ReadName(), "arguments": map[string]any{"cursor": cursor}})
	_, err := tool.Call(context.Background(), raw)
	var invalid *argumentError
	if !errors.As(err, &invalid) || !strings.Contains(err.Error(), "invalid or expired") {
		t.Fatalf("cursor error = %T %v", err, err)
	}
}

func TestConcurrentContinuationOfSameCursorIsStable(t *testing.T) {
	text := strings.Repeat("concurrent synthetic evidence 🙂\n", 3000)
	tool := &QueryTool{Service: &stubRetriever{result: syntheticTextResult(text)}, Workspace: "synthetic"}
	search := resultPage(t, searchCall(t, tool, "concurrent"))
	cursor := *search.NextCursor
	const calls = 8
	results := make(chan []byte, calls)
	errs := make(chan error, calls)
	var wg sync.WaitGroup
	for range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw, _ := json.Marshal(map[string]any{"name": tool.ReadName(), "arguments": map[string]any{"cursor": cursor}})
			result, err := tool.Call(context.Background(), raw)
			if err != nil {
				errs <- err
				return
			}
			encoded, _ := json.Marshal(result)
			results <- encoded
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Errorf("continuation: %v", err)
	}
	var first string
	for encoded := range results {
		if first == "" {
			first = string(encoded)
		} else if string(encoded) != first {
			t.Error("concurrent cursor returned different pages")
		}
	}
}

func TestResultScopedReferencesDoNotCollideAcrossSearches(t *testing.T) {
	tool := &QueryTool{Service: &stubRetriever{result: syntheticResult()}, Workspace: "synthetic"}
	one := resultPage(t, searchCall(t, tool, "one"))
	two := resultPage(t, searchCall(t, tool, "two"))
	if one.ResultID == two.ResultID || one.Packets[0].Reference == two.Packets[0].Reference {
		t.Fatalf("result references collided: %q and %q", one.Packets[0].Reference, two.Packets[0].Reference)
	}
	answer := fmt.Sprintf("Synthetic claims [%s] and [%s].", one.Packets[0].Reference, two.Packets[0].Reference)
	if !strings.Contains(answer, one.ResultID) || !strings.Contains(answer, two.ResultID) {
		t.Fatalf("synthetic answer lost result namespaces: %s", answer)
	}
}

func TestFixedWorkspaceCannotBeSelectedOrCrossed(t *testing.T) {
	one := &stubRetriever{result: syntheticResult()}
	two := &stubRetriever{result: syntheticTextResult("other workspace")}
	toolOne := &QueryTool{Service: one, Workspace: "one"}
	toolTwo := &QueryTool{Service: two, Workspace: "two"}
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"name":"search_one","arguments":{"question":"synthetic","workspace":"two"}}`),
		json.RawMessage(`{"name":"search_two","arguments":{"question":"synthetic"}}`),
	} {
		_, err := toolOne.Call(context.Background(), raw)
		if err == nil {
			t.Fatalf("cross-workspace call accepted: %s", raw)
		}
	}
	if one.got.Question != "" || two.got.Question != "" {
		t.Fatalf("cross-workspace retrieval ran: one=%+v two=%+v", one.got, two.got)
	}
	_ = toolTwo
}

func TestInvalidArgumentsAndFailuresAreSafe(t *testing.T) {
	stub := &stubRetriever{result: syntheticResult()}
	tool := &QueryTool{Service: stub, Workspace: "synthetic"}
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"name":"search_synthetic","arguments":{}}`),
		json.RawMessage(`{"name":"search_synthetic","arguments":{"question":"ok","top_k":0}}`),
		json.RawMessage(`{"name":"read_synthetic_evidence","arguments":{"cursor":"x","start":1}}`),
	} {
		_, err := tool.Call(context.Background(), raw)
		var invalid *argumentError
		if !errors.As(err, &invalid) || !errorResult(err).IsError {
			t.Errorf("Call(%s) error = %T %v", raw, err, err)
		}
	}
	_, err := tool.Call(context.Background(), json.RawMessage(`{"name":"other","arguments":{}}`))
	var unknown *unknownToolError
	if !errors.As(err, &unknown) {
		t.Fatalf("unknown error = %T %v", err, err)
	}

	sensitive := "dial private.example.test:5432: SQL SELECT secret"
	failing := &QueryTool{Service: &stubRetriever{err: errors.New(sensitive)}, Workspace: "synthetic"}
	_, err = failing.Call(context.Background(), json.RawMessage(`{"name":"search_synthetic","arguments":{"question":"synthetic"}}`))
	safe := errorResult(err)
	if !safe.IsError || strings.Contains(safe.Content[0].Text, sensitive) || strings.Contains(safe.Content[0].Text, "SELECT") {
		t.Errorf("client error leaked detail: %q", safe.Content[0].Text)
	}
}

func TestCancelledContinuationDoesNotReadSnapshot(t *testing.T) {
	tool := &QueryTool{Service: &stubRetriever{result: syntheticResult()}, Workspace: "synthetic"}
	page := resultPage(t, searchCall(t, tool, "cancel"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	raw, _ := json.Marshal(map[string]any{"name": tool.ReadName(), "arguments": map[string]any{"cursor": *page.NextCursor}})
	_, err := tool.Call(ctx, raw)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
}

func validateAgainstOutputSchema(t *testing.T, page *EvidencePage) {
	t.Helper()
	schema := compileJSONSchema(t, "evidence-page.schema.json", evidencePageOutputSchema())
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
}

func compileJSONSchema(t *testing.T, name string, document map[string]any) *jsonschema.Schema {
	t.Helper()
	schemaJSON, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	parsed, err := jsonschema.UnmarshalJSON(strings.NewReader(string(schemaJSON)))
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(name, parsed); err != nil {
		t.Fatalf("add %s: %v", name, err)
	}
	schema, err := compiler.Compile(name)
	if err != nil {
		t.Fatalf("compile %s: %v", name, err)
	}
	return schema
}
