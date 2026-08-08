package analysis

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/suankan/pocket-advisor/internal/retrieval"
)

type stubRetriever struct {
	results []*retrieval.Result
	idx     int
	err     error
}

func (s *stubRetriever) Query(_ context.Context, req retrieval.Request) (*retrieval.Result, error) {
	if s.err != nil {
		return nil, s.err
	}
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

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func syntheticPacket(from, subject, snippet string, date time.Time, threadID string) retrieval.Packet {
	docID := fmt.Sprintf("doc-%s-%d", strings.ReplaceAll(from, "@", ""), date.Unix())
	sha256 := strings.Repeat("a", 64)
	return retrieval.Packet{
		Document: retrieval.Document{
			DocID:    docID,
			DocType:  "email",
			Title:    subject,
			From:     from,
			Date:     &date,
			SHA256:   sha256,
			RawURI:   "s3://test/raw/" + docID,
			CharCount: len(snippet),
			ThreadID: threadID,
		},
		Match: retrieval.Match{
			ChunkID:  "chunk-" + docID,
			StartByte: 0,
			EndByte:  len(snippet),
			Score:    0.75,
			Legs:     "both",
			Snippet:  snippet,
		},
		Text: snippet,
	}
}

func TestExecuteTopicAnalysisBasic(t *testing.T) {
	date1 := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	date2 := time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC)
	date3 := time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC)

	result1 := &retrieval.Result{
		Question: "budget review",
		SubQueries: []string{"budget review"},
		Packets: []retrieval.Packet{
			syntheticPacket("alice@test.com", "Budget Update", "The budget is on track for Q2", date1, "thread-1"),
			syntheticPacket("bob@test.com", "Re: Budget Update", "I agree with the projections", date2, "thread-1"),
			syntheticPacket("alice@test.com", "Budget Final", "Budget approved by the board", date3, "thread-2"),
		},
		Warnings: []string{},
		Budget:   retrieval.Budget{BytesUsed: 300, BytesAllowed: 100000},
	}

	stub := &stubRetriever{results: []*retrieval.Result{result1}}
	executor := NewExecutor(stub, testLogger())

	req := TopicRequest{
		Question:   "budget review",
		Participants: []string{"alice@test.com", "bob@test.com"},
	}
	dossier, err := executor.ExecuteTopicAnalysis(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(dossier.EvidenceRefs) == 0 {
		t.Error("expected evidence references")
	}
	if len(dossier.Participants) < 2 {
		t.Errorf("expected at least 2 participants, got %d", len(dossier.Participants))
	}
	if len(dossier.Conversations) == 0 {
		t.Error("expected at least one conversation")
	}
	if len(dossier.Timeline) < 2 {
		t.Errorf("expected at least 2 timeline events, got %d", len(dossier.Timeline))
	}
	if !dossier.Complete {
		t.Error("expected dossier to be complete")
	}
	if dossier.Budget.Used <= 0 {
		t.Error("expected positive budget usage")
	}
}

func TestExecuteTopicAnalysisConflictDetection(t *testing.T) {
	date1 := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	date2 := time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC)

	result1 := &retrieval.Result{
		Question: "project timeline",
		SubQueries: []string{"project timeline"},
		Packets: []retrieval.Packet{
			syntheticPacket("alice@test.com", "Timeline Proposal", "The project will finish by June with a conservative approach", date1, "thread-1"),
			syntheticPacket("alice@test.com", "Timeline Update", "The project deadline has been moved to September for a completely different approach", date2, "thread-1"),
		},
		Warnings: []string{},
		Budget:   retrieval.Budget{BytesUsed: 300, BytesAllowed: 100000},
	}

	stub := &stubRetriever{results: []*retrieval.Result{result1}}
	executor := NewExecutor(stub, testLogger())

	req := TopicRequest{Question: "project timeline"}
	dossier, err := executor.ExecuteTopicAnalysis(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should detect conflicting statements for alice.
	foundConflict := false
	for _, cs := range dossier.ConflictingStatements {
		if cs.Participant == "alice@test.com" {
			foundConflict = true
			break
		}
	}
	if !foundConflict {
		t.Error("expected conflict detection for alice")
	}
}

func TestExecuteTopicAnalysisEmptyResult(t *testing.T) {
	result1 := &retrieval.Result{
		Question: "nonexistent topic",
		SubQueries: []string{"nonexistent topic"},
		Packets:  []retrieval.Packet{},
		Warnings: []string{},
		Budget:   retrieval.Budget{BytesUsed: 0, BytesAllowed: 100000},
	}

	stub := &stubRetriever{results: []*retrieval.Result{result1}}
	executor := NewExecutor(stub, testLogger())

	req := TopicRequest{Question: "nonexistent topic"}
	dossier, err := executor.ExecuteTopicAnalysis(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(dossier.EvidenceRefs) != 0 {
		t.Error("expected no evidence references for empty result")
	}
	if len(dossier.Participants) != 0 {
		t.Error("expected no participants for empty result")
	}

	// Should have no_support_found warning.
	foundNoSupport := false
	for _, w := range dossier.Warnings {
		if w.Kind == WarnNoSupport {
			foundNoSupport = true
			break
		}
	}
	if !foundNoSupport {
		t.Error("expected no_support_found warning")
	}
}

func TestExecuteTopicAnalysisCancellation(t *testing.T) {
	result1 := &retrieval.Result{
		Question: "test",
		Packets: []retrieval.Packet{
			syntheticPacket("a@test.com", "Test", "test", time.Now(), "thread-1"),
		},
		Warnings: []string{},
		Budget:   retrieval.Budget{BytesUsed: 100, BytesAllowed: 100000},
	}

	stub := &stubRetriever{results: []*retrieval.Result{result1}}
	executor := NewExecutor(stub, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	req := TopicRequest{Question: "test"}
	_, err := executor.ExecuteTopicAnalysis(ctx, req)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestExecuteReviewBasic(t *testing.T) {
	executor := NewExecutor(&stubRetriever{}, testLogger())

	items := []CandidateClassification{
		{
			ConversationID: "conv-1",
			Subject:        "Action needed",
			Classification: ClassificationActionRequired,
			Reasoning:      "Contains a direct question requiring response",
		},
		{
			ConversationID: "conv-2",
			Subject:        "FYI only",
			Classification: ClassificationNoActionRequired,
			Reasoning:      "Informational update with no action requested",
		},
		{
			ConversationID: "conv-3",
			Subject:        "Maybe",
			Classification: ClassificationUncertain,
			Reasoning:      "Ambiguous intent unclear",
		},
	}

	req := ReviewRequest{MaxItems: 10}
	dossier, err := executor.ExecuteReview(context.Background(), req, items)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if dossier.TotalCandidates != 3 {
		t.Errorf("expected 3 total candidates, got %d", dossier.TotalCandidates)
	}
	if dossier.ReturnedCount != 3 {
		t.Errorf("expected 3 returned, got %d", dossier.ReturnedCount)
	}
	if !dossier.Complete {
		t.Error("expected complete dossier")
	}

	// Check classifications.
	classifications := make(map[string]int)
	for _, c := range dossier.Classifications {
		classifications[c.Classification]++
	}
	if classifications[ClassificationActionRequired] != 1 {
		t.Error("expected 1 action required")
	}
	if classifications[ClassificationNoActionRequired] != 1 {
		t.Error("expected 1 no action required")
	}
	if classifications[ClassificationUncertain] != 1 {
		t.Error("expected 1 uncertain")
	}
}

func TestExecuteReviewFilterByIDs(t *testing.T) {
	executor := NewExecutor(&stubRetriever{}, testLogger())

	items := []CandidateClassification{
		{ConversationID: "conv-1", Subject: "First"},
		{ConversationID: "conv-2", Subject: "Second"},
		{ConversationID: "conv-3", Subject: "Third"},
	}

	req := ReviewRequest{
		CandidateIDs: []string{"conv-2"},
		MaxItems:     10,
	}
	dossier, err := executor.ExecuteReview(context.Background(), req, items)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if dossier.ReturnedCount != 1 {
		t.Errorf("expected 1 returned, got %d", dossier.ReturnedCount)
	}
	if dossier.Classifications[0].ConversationID != "conv-2" {
		t.Errorf("expected conv-2, got %s", dossier.Classifications[0].ConversationID)
	}
}

func TestExecuteReviewMaxItems(t *testing.T) {
	executor := NewExecutor(&stubRetriever{}, testLogger())

	items := make([]CandidateClassification, 20)
	for i := range items {
		items[i] = CandidateClassification{
			ConversationID: fmt.Sprintf("conv-%d", i),
		}
	}

	req := ReviewRequest{MaxItems: 5}
	dossier, err := executor.ExecuteReview(context.Background(), req, items)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if dossier.ReturnedCount != 5 {
		t.Errorf("expected 5 returned, got %d", dossier.ReturnedCount)
	}
}

func TestExecuteTopicAnalysisInvalidRequest(t *testing.T) {
	executor := NewExecutor(&stubRetriever{}, testLogger())

	req := TopicRequest{Question: ""}
	_, err := executor.ExecuteTopicAnalysis(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for empty question")
	}
}

func TestExecuteReviewInvalidRequest(t *testing.T) {
	executor := NewExecutor(&stubRetriever{}, testLogger())

	req := ReviewRequest{MaxItems: -1}
	_, err := executor.ExecuteReview(context.Background(), req, nil)
	if err == nil {
		t.Fatal("expected error for negative max_items")
	}
}
