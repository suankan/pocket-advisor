package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/suankan/pocket-advisor/internal/retrieval"
	"github.com/suankan/pocket-advisor/internal/statements"
)

type stubBrowser struct {
	mu     sync.Mutex
	result *retrieval.Result
	err    error
	got    statements.Filters
}

func (s *stubBrowser) List(_ context.Context, f statements.Filters) (*retrieval.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = f
	return s.result, s.err
}

func syntheticStatementResult() *retrieval.Result {
	text := "19 Jan 2026 OPENING BALANCE\n18 Apr 2026 CLOSING BALANCE"
	return &retrieval.Result{
		Question:   "bank statement documents matching the given owner, account, and period filters",
		SubQueries: []string{},
		Budget:     retrieval.Budget{BytesUsed: len(text), BytesAllowed: len(text)},
		Packets: []retrieval.Packet{{
			Document: retrieval.Document{
				DocID: "11111111-1111-1111-1111-111111111111", DocType: "pdf", Title: "Statements20260118.pdf",
				RawURI: "s3://synthetic/raw/abc", SHA256: strings.Repeat("a", 64), CharCount: len(text),
			},
			Match: retrieval.Match{ChunkID: "11111111-1111-1111-1111-111111111111", StartByte: 0, EndByte: len(text), Score: 1, Legs: "exact", Snippet: "Suan CBA — period 19 Jan 2026 to 18 Apr 2026"},
			Text:  text,
		}},
	}
}

func newTestStatementsTool(browser StatementBrowser) *QueryTool {
	query := &QueryTool{Workspace: "synthetic"}
	query.Statements = &StatementsTool{Browser: browser, Query: query, Workspace: "synthetic"}
	return query
}

func TestStatementsToolSuccessfulListReturnsFullTextEvidence(t *testing.T) {
	stub := &stubBrowser{result: syntheticStatementResult()}
	query := newTestStatementsTool(stub)

	result, err := query.Call(context.Background(), json.RawMessage(
		`{"name":"list_bank_statements","arguments":{"owner":"suan","bsb":"062018"}}`,
	))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %+v", result)
	}
	if stub.got.Owner != "suan" || stub.got.BSB != "062018" {
		t.Errorf("filters passed through = %+v", stub.got)
	}
	page := resultPage(t, result)
	if len(page.Packets) != 1 {
		t.Fatalf("got %d packets, want 1", len(page.Packets))
	}
	if !page.Packets[0].TextAvailable {
		t.Error("statement text should be available for admission")
	}
}

func TestStatementsToolFiltersPassThroughIncludingPeriod(t *testing.T) {
	stub := &stubBrowser{result: syntheticStatementResult()}
	query := newTestStatementsTool(stub)

	_, err := query.Call(context.Background(), json.RawMessage(
		`{"name":"list_bank_statements","arguments":{"account_name":"cba","since":"2026-01-01","until":"2026-12-31","limit":5}}`,
	))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if stub.got.AccountName != "cba" || stub.got.Limit != 5 {
		t.Errorf("filters passed through = %+v", stub.got)
	}
	if stub.got.Since == nil || stub.got.Since.Format("2006-01-02") != "2026-01-01" {
		t.Errorf("since = %v", stub.got.Since)
	}
	if stub.got.Until == nil || stub.got.Until.Format("2006-01-02") != "2026-12-31" {
		t.Errorf("until = %v", stub.got.Until)
	}
}

func TestStatementsToolEmptyArgumentsListsEverything(t *testing.T) {
	stub := &stubBrowser{result: syntheticStatementResult()}
	query := newTestStatementsTool(stub)

	_, err := query.Call(context.Background(), json.RawMessage(`{"name":"list_bank_statements","arguments":{}}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if stub.got != (statements.Filters{}) {
		t.Errorf("filters = %+v, want the zero value", stub.got)
	}
}

func TestStatementsToolRejectsSinceAfterUntil(t *testing.T) {
	stub := &stubBrowser{result: syntheticStatementResult()}
	query := newTestStatementsTool(stub)

	_, err := query.Call(context.Background(), json.RawMessage(
		`{"name":"list_bank_statements","arguments":{"since":"2026-12-31","until":"2026-01-01"}}`,
	))
	if err == nil {
		t.Fatal("want an error when since is after until")
	}
}

func TestStatementsToolRejectsMalformedDate(t *testing.T) {
	stub := &stubBrowser{result: syntheticStatementResult()}
	query := newTestStatementsTool(stub)

	_, err := query.Call(context.Background(), json.RawMessage(
		`{"name":"list_bank_statements","arguments":{"since":"not-a-date"}}`,
	))
	if err == nil {
		t.Fatal("want an error for a malformed date")
	}
}

func TestStatementsToolRejectsOversizedFilter(t *testing.T) {
	stub := &stubBrowser{result: syntheticStatementResult()}
	query := newTestStatementsTool(stub)
	huge := strings.Repeat("a", maxStatementFilterRunes+1)

	_, err := query.Call(context.Background(), json.RawMessage(
		`{"name":"list_bank_statements","arguments":{"owner":"`+huge+`"}}`,
	))
	if err == nil {
		t.Fatal("want an error for an oversized owner filter")
	}
}

func TestStatementsToolZeroLimitUsesDefault(t *testing.T) {
	// Unlike a validation failure, an absent/zero limit means "use the
	// service default" — the same convention search's top_k uses.
	stub := &stubBrowser{result: syntheticStatementResult()}
	query := newTestStatementsTool(stub)

	_, err := query.Call(context.Background(), json.RawMessage(
		`{"name":"list_bank_statements","arguments":{"limit":0}}`,
	))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if stub.got.Limit != 0 {
		t.Errorf("limit = %d, want 0 passed through for the service to default", stub.got.Limit)
	}
}

func TestStatementsToolRejectsNegativeLimit(t *testing.T) {
	stub := &stubBrowser{result: syntheticStatementResult()}
	query := newTestStatementsTool(stub)

	_, err := query.Call(context.Background(), json.RawMessage(
		`{"name":"list_bank_statements","arguments":{"limit":-1}}`,
	))
	if err == nil {
		t.Fatal("want an error for a negative limit")
	}
}

func TestStatementsToolRejectsLimitAboveMaximum(t *testing.T) {
	stub := &stubBrowser{result: syntheticStatementResult()}
	query := newTestStatementsTool(stub)

	_, err := query.Call(context.Background(), json.RawMessage(
		`{"name":"list_bank_statements","arguments":{"limit":100000}}`,
	))
	if err == nil {
		t.Fatal("want an error for a limit above the maximum")
	}
}

func TestStatementsToolPropagatesBrowserError(t *testing.T) {
	stub := &stubBrowser{err: errUnavailable}
	query := newTestStatementsTool(stub)

	result, err := query.Call(context.Background(), json.RawMessage(`{"name":"list_bank_statements","arguments":{}}`))
	if err == nil {
		t.Fatal("want the browser error to propagate")
	}
	_ = result
}

func TestStatementsToolNilIsUnavailable(t *testing.T) {
	query := &QueryTool{Workspace: "synthetic"}
	// Statements is unset; the name is unknown to this tool without it.
	_, err := query.Call(context.Background(), json.RawMessage(`{"name":"list_bank_statements","arguments":{}}`))
	if err == nil {
		t.Fatal("want an error when list_bank_statements is not configured")
	}
}

func TestStatementsToolIsIncludedInDescribeAll(t *testing.T) {
	query := newTestStatementsTool(&stubBrowser{result: syntheticStatementResult()})
	var found bool
	for _, d := range query.DescribeAll() {
		if d.Name == "list_bank_statements" {
			found = true
		}
	}
	if !found {
		t.Error("list_bank_statements missing from DescribeAll")
	}
}

func TestStatementsToolForCallerGetsIndependentSnapshotStore(t *testing.T) {
	stub := &stubBrowser{result: syntheticStatementResult()}
	original := newTestStatementsTool(stub)
	clone := original.forCaller()

	if clone.Statements == original.Statements {
		t.Fatal("forCaller must give the clone its own StatementsTool, not share the original's")
	}
	if clone.Statements.Query != clone {
		t.Fatal("the clone's StatementsTool must store evidence through the clone itself")
	}

	result, err := clone.Call(context.Background(), json.RawMessage(`{"name":"list_bank_statements","arguments":{}}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	page := resultPage(t, result)
	// The result must be readable from the clone's own read_evidence, proving
	// it was stored in the clone's snapshot store.
	if _, err := clone.readEvidencePage(context.Background(), *page.NextCursor); page.NextCursor != nil && err != nil {
		t.Errorf("clone could not read back its own snapshot: %v", err)
	}
}

var errUnavailable = errors.New("statements browser unavailable")
