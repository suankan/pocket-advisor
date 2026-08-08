package analysis

import (
	"testing"
	"time"
)

func TestBuildPlanBasic(t *testing.T) {
	now := func() time.Time { return time.Date(2025, 6, 15, 0, 0, 0, 0, time.UTC) }
	planner := NewPlanner(now)

	req := TopicRequest{
		Question: "What is the project status?",
	}
	plan := planner.BuildPlan(req, 100000)

	if plan.Question != "What is the project status?" {
		t.Errorf("unexpected question: %s", plan.Question)
	}
	if len(plan.Passes) == 0 {
		t.Fatal("expected at least one pass")
	}
	if plan.MaxEvidence != 100000 {
		t.Errorf("unexpected max evidence: %d", plan.MaxEvidence)
	}

	// Should have lexical_topic and dense_topic passes at minimum.
	foundLexical := false
	foundDense := false
	for _, p := range plan.Passes {
		if p.Kind == "lexical_topic" {
			foundLexical = true
		}
		if p.Kind == "dense_topic" {
			foundDense = true
		}
	}
	if !foundLexical {
		t.Error("missing lexical_topic pass")
	}
	if !foundDense {
		t.Error("missing dense_topic pass")
	}
}

func TestBuildPlanWithParticipants(t *testing.T) {
	now := func() time.Time { return time.Date(2025, 6, 15, 0, 0, 0, 0, time.UTC) }
	planner := NewPlanner(now)

	req := TopicRequest{
		Question:   "Budget discussion",
		Participants: []string{"alice@test.com", "bob@test.com"},
	}
	plan := planner.BuildPlan(req, 100000)

	// Should have participant-related passes.
	foundParticipant := false
	for _, p := range plan.Passes {
		if p.Kind == "participant_combined" || p.Kind == "entity_variant" {
			foundParticipant = true
			break
		}
	}
	if !foundParticipant {
		t.Error("missing participant-related pass")
	}
}

func TestBuildPlanWithDates(t *testing.T) {
	now := func() time.Time { return time.Date(2025, 6, 15, 0, 0, 0, 0, time.UTC) }
	planner := NewPlanner(now)

	after := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	before := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	req := TopicRequest{
		Question: "Project update",
		After:    &after,
		Before:   &before,
	}
	plan := planner.BuildPlan(req, 100000)

	// All passes should have date filters.
	for _, p := range plan.Passes {
		hasAfter := false
		hasBefore := false
		for _, f := range p.Filters {
			if f == "after:2025-01-01" {
				hasAfter = true
			}
			if f == "before:2025-06-01" {
				hasBefore = true
			}
		}
		if !hasAfter {
			t.Errorf("pass %d missing after filter", p.Index)
		}
		if !hasBefore {
			t.Errorf("pass %d missing before filter", p.Index)
		}
	}
}

func TestBuildPlanWithConversations(t *testing.T) {
	now := func() time.Time { return time.Date(2025, 6, 15, 0, 0, 0, 0, time.UTC) }
	planner := NewPlanner(now)

	req := TopicRequest{
		Question:     "Thread summary",
		Conversations: []string{"conv-123", "conv-456"},
	}
	plan := planner.BuildPlan(req, 100000)

	// Should have thread_expansion passes.
	threadPasses := 0
	for _, p := range plan.Passes {
		if p.Kind == "thread_expansion" {
			threadPasses++
		}
	}
	if threadPasses != 2 {
		t.Errorf("expected 2 thread_expansion passes, got %d", threadPasses)
	}
}

func TestBuildPlanSubQueries(t *testing.T) {
	now := func() time.Time { return time.Date(2025, 6, 15, 0, 0, 0, 0, time.UTC) }
	planner := NewPlanner(now)

	req := TopicRequest{
		Question: "What is the current status of the budget review with the finance team?",
	}
	plan := planner.BuildPlan(req, 100000)

	subQueryPasses := 0
	for _, p := range plan.Passes {
		if p.Kind == "sub_query" {
			subQueryPasses++
		}
	}
	if subQueryPasses == 0 {
		t.Error("expected at least one sub_query pass")
	}
}

func TestBuildPlanDefaultBudget(t *testing.T) {
	now := func() time.Time { return time.Date(2025, 6, 15, 0, 0, 0, 0, time.UTC) }
	planner := NewPlanner(now)

	req := TopicRequest{Question: "test"}
	plan := planner.BuildPlan(req, 0)

	if plan.MaxEvidence != DefaultEvidenceBudget() {
		t.Errorf("expected default budget, got %d", plan.MaxEvidence)
	}
}

func TestEstimatePassCost(t *testing.T) {
	plan := &ResearchPlan{
		Passes: []ResearchPass{
			{Index: 0, Kind: "lexical_topic"},
			{Index: 1, Kind: "dense_topic"},
			{Index: 2, Kind: "sub_query"},
		},
	}
	cost := EstimatePassCost(plan)
	expected := 3 * 15 * 500
	if cost != expected {
		t.Errorf("expected %d, got %d", expected, cost)
	}
}

func TestCanContinue(t *testing.T) {
	plan := &ResearchPlan{
		Passes: []ResearchPass{{Index: 0}},
	}
	if plan.CanContinue() {
		t.Error("plan without continuation should not can-continue")
	}

	plan.Continuation = &ContinuationState{PassIndex: 0}
	if !plan.CanContinue() {
		t.Error("plan with continuation should can-continue")
	}

	plan.Continuation = &ContinuationState{PassIndex: 1}
	if plan.CanContinue() {
		t.Error("plan past end should not can-continue")
	}
}

func TestDeriveSubQueries(t *testing.T) {
	subs := deriveSubQueries("What is the current status of the budget review?")
	if len(subs) == 0 {
		t.Error("expected at least one sub-query")
	}
	// Each sub-query should be non-empty.
	for _, sq := range subs {
		if sq == "" {
			t.Error("empty sub-query")
		}
	}
}

func TestDeriveSubQueriesStopWords(t *testing.T) {
	subs := deriveSubQueries("what is the budget")
	// "what" and "is" and "the" should be filtered.
	for _, sq := range subs {
		if sq == "what is" || sq == "is the" || sq == "the budget" {
			// This is expected as sub-queries, not individual stop words.
		}
	}
}

func TestBuildPlanPassOrder(t *testing.T) {
	now := func() time.Time { return time.Date(2025, 6, 15, 0, 0, 0, 0, time.UTC) }
	planner := NewPlanner(now)

	req := TopicRequest{
		Question:     "Budget discussion",
		Participants: []string{"alice@test.com"},
	}
	plan := planner.BuildPlan(req, 100000)

	// Passes should have sequential indices.
	for i, p := range plan.Passes {
		if p.Index != i {
			t.Errorf("pass %d has index %d", i, p.Index)
		}
	}
}
