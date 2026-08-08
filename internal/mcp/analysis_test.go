package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/suankan/pocket-advisor/internal/analysis"
	"github.com/suankan/pocket-advisor/internal/retrieval"
)

func analysisLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

type analysisStubRetriever struct {
	results []*retrieval.Result
	idx     int
}

func (s *analysisStubRetriever) Query(_ context.Context, req retrieval.Request) (*retrieval.Result, error) {
	if s.idx >= len(s.results) {
		return &retrieval.Result{
			Question: req.Question, Packets: []retrieval.Packet{},
			Warnings: []string{}, Budget: retrieval.Budget{BytesAllowed: 100000},
		}, nil
	}
	result := s.results[s.idx]
	s.idx++
	return result, nil
}

func analysisSyntheticResult() *retrieval.Result {
	date := time.Date(2025, 3, 15, 10, 0, 0, 0, time.UTC)
	sha := strings.Repeat("a", 64)
	return &retrieval.Result{
		Question:   "budget review",
		SubQueries: []string{"budget review"},
		Packets: []retrieval.Packet{{
			Document: retrieval.Document{
				DocID:    "doc-11111111-1111-1111-1111-111111111111",
				DocType:  "email",
				Title:    "Budget Update",
				From:     "alice@test.com",
				Date:     &date,
				SHA256:   sha,
				RawURI:   "s3://test/raw/doc1",
				CharCount: 50,
			},
			Match: retrieval.Match{
				ChunkID:  "chunk-11111111-1111-1111-1111-111111111111",
				StartByte: 0,
				EndByte:  50,
				Score:    0.8,
				Legs:     "both",
				Snippet:  "The budget review is progressing well",
			},
			Text: "The budget review is progressing well and all milestones are on track.",
		}},
		Warnings: []string{},
		Budget:   retrieval.Budget{BytesUsed: 60, BytesAllowed: 120000},
	}
}

func newAnalysisTool(stub *analysisStubRetriever) *AnalysisTool {
	retriever := &QueryTool{
		Service:   stub,
		Workspace: "test",
		Title:     "Test Workspace",
		Corpus:    []string{"test emails"},
	}

	executor := analysis.NewExecutor(stub, analysisLogger())

	return &AnalysisTool{
		QueryTool: retriever,
		Executor:  executor,
	}
}

func TestAnalyzeTopicToolName(t *testing.T) {
	stub := &analysisStubRetriever{}
	tool := newAnalysisTool(stub)
	name := tool.AnalysisToolName()
	if name != "analyze_topic_test" {
		t.Errorf("unexpected tool name: %s", name)
	}
}

func TestReviewToolName(t *testing.T) {
	stub := &analysisStubRetriever{}
	tool := newAnalysisTool(stub)
	name := tool.ReviewToolName()
	if name != "review_awaiting_reply_test" {
		t.Errorf("unexpected tool name: %s", name)
	}
}

func TestDescribeAnalysis(t *testing.T) {
	stub := &analysisStubRetriever{}
	tool := newAnalysisTool(stub)
	def := tool.DescribeAnalysis()
	if def.Name != "analyze_topic_test" {
		t.Errorf("unexpected tool name: %s", def.Name)
	}
	if def.InputSchema == nil {
		t.Error("missing input schema")
	}
	if def.OutputSchema == nil {
		t.Error("missing output schema")
	}
	if !def.Annotations.ReadOnlyHint {
		t.Error("expected read-only annotation")
	}
}

func TestDescribeReview(t *testing.T) {
	stub := &analysisStubRetriever{}
	tool := newAnalysisTool(stub)
	def := tool.DescribeReview()
	if def.Name != "review_awaiting_reply_test" {
		t.Errorf("unexpected tool name: %s", def.Name)
	}
}

func TestDescribeAllAnalysis(t *testing.T) {
	stub := &analysisStubRetriever{}
	tool := newAnalysisTool(stub)
	defs := tool.DescribeAllAnalysis()
	// Should have 4 tools: search, read, analyze_topic, review_awaiting_reply
	if len(defs) != 4 {
		t.Errorf("expected 4 tool definitions, got %d", len(defs))
	}
	names := make(map[string]bool)
	for _, d := range defs {
		names[d.Name] = true
	}
	if !names["search_test"] {
		t.Error("missing search tool")
	}
	if !names["read_test_evidence"] {
		t.Error("missing read tool")
	}
	if !names["analyze_topic_test"] {
		t.Error("missing analyze_topic tool")
	}
	if !names["review_awaiting_reply_test"] {
		t.Error("missing review_awaiting_reply tool")
	}
}

func TestCallAnalyzeTopic(t *testing.T) {
	stub := &analysisStubRetriever{results: []*retrieval.Result{analysisSyntheticResult()}}
	tool := newAnalysisTool(stub)

	raw, err := json.Marshal(map[string]any{
		"name":      tool.AnalysisToolName(),
		"arguments": map[string]any{"question": "What is the budget status?"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.CallAnalysis(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %+v", result)
	}
	if result.StructuredContent == nil {
		t.Fatal("expected structured content")
	}
	// Verify it's a TopicDossier
	dossier, ok := result.StructuredContent.(*analysis.TopicDossier)
	if !ok {
		t.Fatalf("unexpected structured content type: %T", result.StructuredContent)
	}
	if dossier.Budget.PassesRun == 0 {
		t.Error("expected at least one pass run")
	}
}

func TestCallAnalyzeTopicEmptyQuestion(t *testing.T) {
	stub := &analysisStubRetriever{}
	tool := newAnalysisTool(stub)

	raw, err := json.Marshal(map[string]any{
		"name":      tool.AnalysisToolName(),
		"arguments": map[string]any{"question": ""},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tool.CallAnalysis(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error for empty question")
	}
	// The error should be an argumentError
	var argErr *argumentError
	if !errors.As(err, &argErr) {
		t.Errorf("expected argumentError, got %T: %v", err, err)
	}
}

func TestCallReviewAwaitingReply(t *testing.T) {
	stub := &analysisStubRetriever{}
	tool := newAnalysisTool(stub)
	tool.OutboxProvider = func(_ context.Context) ([]analysis.CandidateClassification, error) {
		return []analysis.CandidateClassification{
			{
				ConversationID: "conv-1",
				Subject:        "Need response",
				Classification: analysis.ClassificationActionRequired,
				Reasoning:      "Direct question in email",
			},
		}, nil
	}

	raw, err := json.Marshal(map[string]any{
		"name":      tool.ReviewToolName(),
		"arguments": map[string]any{"max_items": 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.CallAnalysis(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %+v", result)
	}
	// Verify it's a ReviewDossier
	dossier, ok := result.StructuredContent.(*analysis.ReviewDossier)
	if !ok {
		t.Fatalf("unexpected structured content type: %T", result.StructuredContent)
	}
	if dossier.ReturnedCount != 1 {
		t.Errorf("expected 1 returned, got %d", dossier.ReturnedCount)
	}
}

func TestCallReviewAwaitingReplyEmptyOutbox(t *testing.T) {
	stub := &analysisStubRetriever{}
	tool := newAnalysisTool(stub)
	tool.OutboxProvider = func(_ context.Context) ([]analysis.CandidateClassification, error) {
		return nil, nil
	}

	raw, err := json.Marshal(map[string]any{
		"name":      tool.ReviewToolName(),
		"arguments": map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.CallAnalysis(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %+v", result)
	}
}

func TestCallAnalysisDelegatesSearch(t *testing.T) {
	stub := &analysisStubRetriever{results: []*retrieval.Result{analysisSyntheticResult()}}
	tool := newAnalysisTool(stub)

	raw, err := json.Marshal(map[string]any{
		"name":      tool.Name(),
		"arguments": map[string]any{"question": "test query"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.CallAnalysis(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %+v", result)
	}
}

func TestCallAnalysisUnknownTool(t *testing.T) {
	stub := &analysisStubRetriever{}
	tool := newAnalysisTool(stub)

	raw, err := json.Marshal(map[string]any{
		"name":      "unknown_tool",
		"arguments": map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tool.CallAnalysis(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestParseFlexibleDate(t *testing.T) {
	tests := []struct {
		input string
		want  string
		err   bool
	}{
		{"2025-03-15", "2025-03-15 00:00:00 +0000 UTC", false},
		{"2025-03-15T10:30:00Z", "2025-03-15 10:30:00 +0000 UTC", false},
		{"invalid", "", true},
		{"", "", true},
	}

	for _, tt := range tests {
		tm, err := parseFlexibleDate(tt.input)
		if tt.err {
			if err == nil {
				t.Errorf("parseFlexibleDate(%q) expected error", tt.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseFlexibleDate(%q) unexpected error: %v", tt.input, err)
			continue
		}
		if tm.Format("2006-01-02 15:04:05 -0700 MST") != tt.want {
			t.Errorf("parseFlexibleDate(%q) = %v, want %s", tt.input, tm, tt.want)
		}
	}
}

func TestRenderDossier(t *testing.T) {
	dossier := &analysis.TopicDossier{
		Request: analysis.TopicRequest{Question: "test question"},
		Budget: analysis.EvidenceBudgetInfo{
			Used:    500,
			Allowed: 100000,
			Unit:    analysis.UTF8ByteUnit,
		},
		EvidenceRefs: []analysis.EvidenceReference{
			{ResultID: "A12345678901", Rank: 1, FullRef: "A12345678901:E1"},
			{ResultID: "A12345678901", Rank: 2, FullRef: "A12345678901:E2"},
		},
		Warnings: []analysis.Warning{
			{Kind: analysis.WarnNoSupport, Message: "no evidence found"},
		},
		Complete: true,
	}

	text := renderDossier(dossier)
	if !strings.Contains(text, "test question") {
		t.Error("rendered text should contain the question")
	}
	if !strings.Contains(text, "A12345678901:E1") {
		t.Error("rendered text should contain evidence references")
	}
	if !strings.Contains(text, "Delivery complete") {
		t.Error("rendered text should indicate completion")
	}
}

func TestRenderReviewDossier(t *testing.T) {
	dossier := &analysis.ReviewDossier{
		TotalCandidates: 3,
		ReturnedCount:   2,
		Classifications: []analysis.CandidateClassification{
			{
				ConversationID: "conv-1",
				Subject:        "Test",
				Classification: analysis.ClassificationActionRequired,
				Reasoning:      "Contains question",
			},
		},
		Complete: true,
	}

	text := renderReviewDossier(dossier)
	if !strings.Contains(text, "3 candidate(s)") {
		t.Error("rendered text should contain candidate count")
	}
	if !strings.Contains(text, "likely_action_required") {
		t.Error("rendered text should contain classification")
	}
}

func TestAnalysisInputSchema(t *testing.T) {
	schema := analyzeInputSchema()
	if schema == nil {
		t.Fatal("nil schema")
	}
	if _, ok := schema["properties"]; !ok {
		t.Error("missing properties")
	}
	props := schema["properties"].(map[string]any)
	if _, ok := props["question"]; !ok {
		t.Error("missing question property")
	}
	if _, ok := props["participants"]; !ok {
		t.Error("missing participants property")
	}
}

func TestReviewInputSchema(t *testing.T) {
	schema := reviewInputSchema()
	if schema == nil {
		t.Fatal("nil schema")
	}
	if _, ok := schema["properties"]; !ok {
		t.Error("missing properties")
	}
	props := schema["properties"].(map[string]any)
	if _, ok := props["candidate_ids"]; !ok {
		t.Error("missing candidate_ids property")
	}
	if _, ok := props["max_items"]; !ok {
		t.Error("missing max_items property")
	}
}

func TestAnalyzeTopicWithDates(t *testing.T) {
	stub := &analysisStubRetriever{results: []*retrieval.Result{analysisSyntheticResult()}}
	tool := newAnalysisTool(stub)

	raw, err := json.Marshal(map[string]any{
		"name": tool.AnalysisToolName(),
		"arguments": map[string]any{
			"question": "budget review",
			"after":    "2025-01-01",
			"before":   "2025-12-31",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.CallAnalysis(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %+v", result)
	}
	dossier, ok := result.StructuredContent.(*analysis.TopicDossier)
	if !ok {
		t.Fatalf("unexpected type: %T", result.StructuredContent)
	}
	if dossier.Scope.After == nil || dossier.Scope.Before == nil {
		t.Error("expected date scope to be set")
	}
}

func TestAnalyzeTopicWithParticipants(t *testing.T) {
	stub := &analysisStubRetriever{results: []*retrieval.Result{analysisSyntheticResult()}}
	tool := newAnalysisTool(stub)

	raw, err := json.Marshal(map[string]any{
		"name": tool.AnalysisToolName(),
		"arguments": map[string]any{
			"question":     "budget review",
			"participants": []string{"alice@test.com", "bob@test.com"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.CallAnalysis(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %+v", result)
	}
	dossier, ok := result.StructuredContent.(*analysis.TopicDossier)
	if !ok {
		t.Fatalf("unexpected type: %T", result.StructuredContent)
	}
	if len(dossier.Scope.Participants) != 2 {
		t.Errorf("expected 2 participants in scope, got %d", len(dossier.Scope.Participants))
	}
}
