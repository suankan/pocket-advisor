package analysis

import (
	"testing"
	"time"
)

func TestValidateTopicRequestEmptyQuestion(t *testing.T) {
	req := TopicRequest{Question: ""}
	if err := ValidateTopicRequest(&req); err == nil {
		t.Fatal("expected error for empty question")
	}
}

func TestValidateTopicRequestQuestionTooLong(t *testing.T) {
	req := TopicRequest{Question: string(make([]rune, MaxTopicRunes+1))}
	if err := ValidateTopicRequest(&req); err == nil {
		t.Fatal("expected error for question exceeding max runes")
	}
}

func TestValidateTopicRequestAfterBeforeMismatch(t *testing.T) {
	after := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	before := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	req := TopicRequest{Question: "test", After: &after, Before: &before}
	if err := ValidateTopicRequest(&req); err == nil {
		t.Fatal("expected error when after > before")
	}
}

func TestValidateTopicRequestValid(t *testing.T) {
	after := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	before := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	req := TopicRequest{
		Question:   "What is the status of the project?",
		Participants: []string{"alice@example.com"},
		After:      &after,
		Before:     &before,
	}
	if err := ValidateTopicRequest(&req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateTopicRequestTooManyParticipants(t *testing.T) {
	participants := make([]string, MaxParticipants+1)
	for i := range participants {
		participants[i] = "user@example.com"
	}
	req := TopicRequest{Question: "test", Participants: participants}
	if err := ValidateTopicRequest(&req); err == nil {
		t.Fatal("expected error for too many participants")
	}
}

func TestValidateReviewRequestNegativeMaxItems(t *testing.T) {
	req := ReviewRequest{MaxItems: -1}
	if err := ValidateReviewRequest(&req); err == nil {
		t.Fatal("expected error for negative max_items")
	}
}

func TestValidateReviewRequestValid(t *testing.T) {
	req := ReviewRequest{MaxItems: 10}
	if err := ValidateReviewRequest(&req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSortParticipantsByLatest(t *testing.T) {
	parts := []ParticipantObservation{
		{Address: "a@test.com", LatestDate: "2025-01-01"},
		{Address: "b@test.com", LatestDate: "2025-06-01"},
		{Address: "c@test.com", LatestDate: "2025-03-01"},
	}
	SortParticipantsByLatest(parts)
	if parts[0].Address != "b@test.com" {
		t.Errorf("expected b@test.com first, got %s", parts[0].Address)
	}
	if parts[2].Address != "a@test.com" {
		t.Errorf("expected a@test.com last, got %s", parts[2].Address)
	}
}

func TestSortTimelineChronological(t *testing.T) {
	events := []TimelineEvent{
		{Date: "2025-06-01T00:00:00Z"},
		{Date: "2025-01-01T00:00:00Z"},
		{Date: "2025-03-01T00:00:00Z"},
	}
	SortTimelineChronological(events)
	if events[0].Date != "2025-01-01T00:00:00Z" {
		t.Errorf("expected earliest first, got %s", events[0].Date)
	}
	if events[2].Date != "2025-06-01T00:00:00Z" {
		t.Errorf("expected latest last, got %s", events[2].Date)
	}
}

func TestDefaultEvidenceBudget(t *testing.T) {
	budget := DefaultEvidenceBudget()
	if budget != MaxEvidenceBudget {
		t.Errorf("expected %d, got %d", MaxEvidenceBudget, budget)
	}
}

func TestDefaultMaxItems(t *testing.T) {
	maxItems := DefaultMaxItems()
	if maxItems != 15 {
		t.Errorf("expected 15, got %d", maxItems)
	}
}
