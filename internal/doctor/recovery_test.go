package doctor

import (
	"context"
	"testing"
	"time"

	"github.com/suankan/pocket-advisor/internal/domain"
)

type fakeBus struct {
	msgs uint64
}

func (f *fakeBus) StreamInfo(_ context.Context, _ string) (uint64, error) {
	return f.msgs, nil
}

func TestClassifyStalePending(t *testing.T) {
	p := NewPlanner(RecoverConfig{WorkspaceID: "ws"}, []RecoveryItem{
		{DocID: "doc-1", Status: domain.StatusPending},
	}, nil)
	plan := p.Plan(context.Background())
	if len(plan.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(plan.Items))
	}
	item := plan.Items[0]
	if item.Classification != ClassRetryable {
		t.Errorf("classification %q, want retryable", item.Classification)
	}
	if item.Action != ActionRetryTransition {
		t.Errorf("action %q, want transition_to_pending", item.Action)
	}
	if item.MessagePublish == "" {
		t.Error("expected message publish description")
	}
}

func TestClassifyStaleProcessing(t *testing.T) {
	p := NewPlanner(RecoverConfig{WorkspaceID: "ws"}, []RecoveryItem{
		{DocID: "doc-2", Status: domain.StatusProcessing},
	}, nil)
	plan := p.Plan(context.Background())
	item := plan.Items[0]
	if item.Classification != ClassRetryable {
		t.Errorf("classification %q, want retryable", item.Classification)
	}
	if item.Action != ActionResetProcessing {
		t.Errorf("action %q, want reset_to_pending", item.Action)
	}
}

func TestClassifyRetryableFailed(t *testing.T) {
	p := NewPlanner(RecoverConfig{WorkspaceID: "ws"}, []RecoveryItem{
		{DocID: "doc-3", Status: domain.StatusFailed, Reason: domain.ReasonExtractionFailed},
	}, nil)
	plan := p.Plan(context.Background())
	item := plan.Items[0]
	if item.Classification != ClassRetryable {
		t.Errorf("classification %q, want retryable", item.Classification)
	}
}

func TestClassifyTerminalFailed(t *testing.T) {
	p := NewPlanner(RecoverConfig{WorkspaceID: "ws"}, []RecoveryItem{
		{DocID: "doc-4", Status: domain.StatusFailed, Reason: domain.ReasonMalformedCommand},
	}, nil)
	plan := p.Plan(context.Background())
	item := plan.Items[0]
	if item.Classification != ClassTerminal {
		t.Errorf("classification %q, want terminal", item.Classification)
	}
	if item.Action != ActionRefuse {
		t.Errorf("action %q, want refused", item.Action)
	}
}

func TestClassifyUnclassifiedFailed(t *testing.T) {
	p := NewPlanner(RecoverConfig{WorkspaceID: "ws"}, []RecoveryItem{
		{DocID: "doc-5", Status: domain.StatusFailed, Reason: domain.ReasonUnclassified},
	}, nil)
	plan := p.Plan(context.Background())
	item := plan.Items[0]
	if item.Classification != ClassNotRecoverable {
		t.Errorf("classification %q, want not_recoverable", item.Classification)
	}
}

func TestClassifyCompletedIsConverged(t *testing.T) {
	p := NewPlanner(RecoverConfig{WorkspaceID: "ws"}, []RecoveryItem{
		{DocID: "doc-6", Status: domain.StatusCompleted},
	}, nil)
	plan := p.Plan(context.Background())
	item := plan.Items[0]
	if item.Classification != ClassConverged {
		t.Errorf("classification %q, want converged", item.Classification)
	}
	if item.Action != ActionSkip {
		t.Errorf("action %q, want no_action_needed", item.Action)
	}
}

func TestClassifySkippedIsConverged(t *testing.T) {
	p := NewPlanner(RecoverConfig{WorkspaceID: "ws"}, []RecoveryItem{
		{DocID: "doc-7", Status: domain.StatusSkipped, Reason: domain.ReasonUnsupportedFormat},
	}, nil)
	plan := p.Plan(context.Background())
	item := plan.Items[0]
	if item.Classification != ClassConverged {
		t.Errorf("classification %q, want converged", item.Classification)
	}
}

func TestPlanSummary(t *testing.T) {
	p := NewPlanner(RecoverConfig{WorkspaceID: "ws"}, []RecoveryItem{
		{DocID: "a", Status: domain.StatusPending},
		{DocID: "b", Status: domain.StatusFailed, Reason: domain.ReasonMalformedCommand},
		{DocID: "c", Status: domain.StatusCompleted},
	}, nil)
	plan := p.Plan(context.Background())
	if plan.Summary == "" {
		t.Error("expected non-empty summary")
	}
	if plan.WorkspaceID != "ws" {
		t.Errorf("workspace %q, want ws", plan.WorkspaceID)
	}
	if !plan.DryRun {
		t.Error("expected dry run by default")
	}
}

func TestValidateRefusesBroadTerminalRedrive(t *testing.T) {
	p := NewPlanner(RecoverConfig{WorkspaceID: "ws"}, nil, nil)
	plan := &RecoveryPlan{
		Items: []RecoveryItem{
			{DocID: "x", Classification: ClassTerminal, Action: ActionRetryTransition},
		},
	}
	err := p.Validate(context.Background(), plan)
	if err == nil {
		t.Error("expected validation error for terminal redrive")
	}
}

func TestValidatePassesWithAllRefused(t *testing.T) {
	p := NewPlanner(RecoverConfig{WorkspaceID: "ws"}, nil, nil)
	plan := &RecoveryPlan{
		Items: []RecoveryItem{
			{DocID: "x", Classification: ClassTerminal, Action: ActionRefuse},
		},
	}
	if err := p.Validate(context.Background(), plan); err != nil {
		t.Errorf("unexpected validation error: %v", err)
	}
}

func TestValidateWarnsOnBusyStream(t *testing.T) {
	p := NewPlanner(RecoverConfig{WorkspaceID: "ws"}, nil, &fakeBus{msgs: 5000})
	plan := &RecoveryPlan{
		Items: []RecoveryItem{
			{DocID: "x", Classification: ClassRetryable, Action: ActionRetryTransition},
		},
	}
	err := p.Validate(context.Background(), plan)
	if err == nil {
		t.Error("expected validation warning for busy stream")
	}
}

func TestValidatePassesOnQuietStream(t *testing.T) {
	p := NewPlanner(RecoverConfig{WorkspaceID: "ws"}, nil, &fakeBus{msgs: 10})
	plan := &RecoveryPlan{
		Items: []RecoveryItem{
			{DocID: "x", Classification: ClassRetryable, Action: ActionRetryTransition},
		},
	}
	if err := p.Validate(context.Background(), plan); err != nil {
		t.Errorf("unexpected validation error: %v", err)
	}
}

func TestMixedPlanClassifications(t *testing.T) {
	now := time.Now()
	p := NewPlanner(RecoverConfig{WorkspaceID: "ws"}, []RecoveryItem{
		{DocID: "a", Status: domain.StatusPending},
		{DocID: "b", Status: domain.StatusProcessing},
		{DocID: "c", Status: domain.StatusFailed, Reason: domain.ReasonExtractionFailed},
		{DocID: "d", Status: domain.StatusFailed, Reason: domain.ReasonMalformedCommand},
		{DocID: "e", Status: domain.StatusCompleted},
		{DocID: "f", Status: domain.StatusSkipped, Reason: domain.ReasonUnsupportedFormat},
	}, nil)
	plan := p.Plan(context.Background())
	if len(plan.Items) != 6 {
		t.Fatalf("expected 6 items, got %d", len(plan.Items))
	}

	counts := map[RecoveryClassification]int{}
	for _, item := range plan.Items {
		counts[item.Classification]++
	}
	if counts[ClassRetryable] != 3 {
		t.Errorf("retryable %d, want 3", counts[ClassRetryable])
	}
	if counts[ClassTerminal] != 1 {
		t.Errorf("terminal %d, want 1", counts[ClassTerminal])
	}
	if counts[ClassConverged] != 2 {
		t.Errorf("converged %d, want 2", counts[ClassConverged])
	}
	_ = now
}
