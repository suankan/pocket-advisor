package doctor

import (
	"context"
	"testing"
	"time"

	"github.com/suankan/pocket-advisor/internal/domain"
)

func TestClassifyStalePending(t *testing.T) {
	p := NewPlanner(RecoverConfig{WorkspaceID: "ws"}, []RecoveryItem{
		{DocID: "doc-1", Status: domain.StatusPending},
	})
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
	if item.Requeue == "" {
		t.Error("expected a requeue description")
	}
}

func TestClassifyStaleProcessing(t *testing.T) {
	p := NewPlanner(RecoverConfig{WorkspaceID: "ws"}, []RecoveryItem{
		{DocID: "doc-2", Status: domain.StatusProcessing},
	})
	plan := p.Plan(context.Background())
	item := plan.Items[0]
	if item.Classification != ClassRetryable {
		t.Errorf("classification %q, want retryable", item.Classification)
	}
	if item.Action != ActionResetProcessing {
		t.Errorf("action %q, want reset_to_pending", item.Action)
	}
}

// A stale document whose jobs are all still queued has nothing for recovery
// to reset, so it is reported as needing no action; one with a failed or
// expired job is still retried.
func TestClassifyAlreadyQueuedNeedsNoAction(t *testing.T) {
	p := NewPlanner(RecoverConfig{WorkspaceID: "ws"}, []RecoveryItem{
		{DocID: "queued", Status: domain.StatusPending, Jobs: []JobRef{{ID: "j1", State: "READY"}}},
		{DocID: "waiting", Status: domain.StatusProcessing, Jobs: []JobRef{{ID: "j2", State: "READY"}, {ID: "j3", State: "WAITING_SOURCE"}}},
		{DocID: "failed", Status: domain.StatusPending, Jobs: []JobRef{{ID: "j4", State: "READY"}, {ID: "j5", State: "FAILED"}}},
		{DocID: "expired", Status: domain.StatusProcessing, Jobs: []JobRef{{ID: "j6", State: "RUNNING"}}},
	})
	got := map[string]RecoveryItem{}
	for _, item := range p.Plan(context.Background()).Items {
		got[item.DocID] = item
	}
	for doc, want := range map[string]RecoveryAction{
		"queued": ActionSkip, "waiting": ActionSkip, "failed": ActionRetryTransition, "expired": ActionResetProcessing,
	} {
		if got[doc].Action != want {
			t.Errorf("%s: action %q, want %q", doc, got[doc].Action, want)
		}
	}
	if got["queued"].Classification != ClassConverged || got["queued"].Requeue == "" {
		t.Errorf("queued: %+v", got["queued"])
	}
}

func TestClassifyRetryableFailed(t *testing.T) {
	p := NewPlanner(RecoverConfig{WorkspaceID: "ws"}, []RecoveryItem{
		{DocID: "doc-3", Status: domain.StatusFailed, Reason: domain.ReasonExtractionFailed},
	})
	plan := p.Plan(context.Background())
	item := plan.Items[0]
	if item.Classification != ClassRetryable {
		t.Errorf("classification %q, want retryable", item.Classification)
	}
}

// A decline is recorded SKIPPED, so a failure carrying one came from a handler
// that predates the decline: it is run again, not taken as settled.
func TestClassifyFailedDeclineIsRedriven(t *testing.T) {
	p := NewPlanner(RecoverConfig{WorkspaceID: "ws"}, []RecoveryItem{
		{DocID: "doc-8", Status: domain.StatusFailed, Reason: domain.ReasonRecursionLimit},
	})
	plan := p.Plan(context.Background())
	item := plan.Items[0]
	if item.Classification != ClassRetryable || item.Action != ActionRetryTransition {
		t.Errorf("classification %q action %q, want retryable transition_to_pending", item.Classification, item.Action)
	}
	if err := p.Validate(context.Background(), plan); err != nil {
		t.Errorf("a redriven decline failed validation: %v", err)
	}
}

// A failure with no job was recorded before the job queue, so its reason, even
// a terminal or unclassified one, is no verdict on the current handlers.
func TestClassifyJoblessFailedIsRedriven(t *testing.T) {
	p := NewPlanner(RecoverConfig{WorkspaceID: "ws"}, []RecoveryItem{
		{DocID: "doc-9", Status: domain.StatusFailed, Reason: domain.ReasonUnclassified, Jobless: true},
		{DocID: "doc-10", Status: domain.StatusFailed, Reason: domain.ReasonUnknownEncoding, Jobless: true},
	})
	plan := p.Plan(context.Background())
	for _, item := range plan.Items {
		if item.Classification != ClassRetryable || item.Action != ActionRetryTransition {
			t.Errorf("%s: classification %q action %q, want retryable transition_to_pending", item.DocID, item.Classification, item.Action)
		}
	}
	if err := p.Validate(context.Background(), plan); err != nil {
		t.Errorf("a jobless failure failed validation: %v", err)
	}
}

// A reason the operator names is their judgment: its failures are rerun
// whatever the reason's class, and only those named.
func TestClassifyNamedReasonIsRedriven(t *testing.T) {
	docs := []RecoveryItem{
		{DocID: "named", Status: domain.StatusFailed, Reason: domain.ReasonUnclassified},
		{DocID: "other", Status: domain.StatusFailed, Reason: domain.ReasonHandlerPanic},
	}
	p := NewPlanner(RecoverConfig{WorkspaceID: "ws", FailureReas: []domain.FailureReason{domain.ReasonUnclassified}}, docs)
	plan := p.Plan(context.Background())
	if c := plan.Items[0].Classification; c != ClassRetryable || plan.Items[0].Action != ActionRetryTransition {
		t.Errorf("named reason planned %q %q", c, plan.Items[0].Action)
	}
	if c := plan.Items[1].Classification; c != ClassTerminal {
		t.Errorf("unnamed terminal reason planned %q", c)
	}
	if c := NewPlanner(RecoverConfig{WorkspaceID: "ws"}, docs[:1]).Plan(context.Background()).Items[0].Classification; c != ClassNotRecoverable {
		t.Errorf("unnamed UNCLASSIFIED planned %q", c)
	}
}

// Choosing all reasons reruns every failure, however it is classified, and
// leaves other states to their own rules.
func TestClassifyAllFailedIsRedriven(t *testing.T) {
	p := NewPlanner(RecoverConfig{WorkspaceID: "ws", AllFailed: true}, []RecoveryItem{
		{DocID: "a", Status: domain.StatusFailed, Reason: domain.ReasonHandlerPanic},
		{DocID: "b", Status: domain.StatusFailed, Reason: domain.ReasonUnclassified},
		{DocID: "c", Status: domain.StatusFailed, Reason: "SOMETHING_UNKNOWN"},
		{DocID: "d", Status: domain.StatusSkipped, Reason: domain.ReasonUnsupportedFormat},
	})
	plan := p.Plan(context.Background())
	for _, item := range plan.Items[:3] {
		if item.Classification != ClassRetryable || item.Action != ActionRetryTransition {
			t.Errorf("%s planned %q %q", item.DocID, item.Classification, item.Action)
		}
	}
	if plan.Items[3].Classification != ClassConverged {
		t.Errorf("a skipped document planned %q", plan.Items[3].Classification)
	}
	if err := p.Validate(context.Background(), plan); err != nil {
		t.Errorf("validation: %v", err)
	}
}

func TestClassifyTerminalFailed(t *testing.T) {
	p := NewPlanner(RecoverConfig{WorkspaceID: "ws"}, []RecoveryItem{
		{DocID: "doc-4", Status: domain.StatusFailed, Reason: domain.ReasonMalformedCommand},
	})
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
	})
	plan := p.Plan(context.Background())
	item := plan.Items[0]
	if item.Classification != ClassNotRecoverable {
		t.Errorf("classification %q, want not_recoverable", item.Classification)
	}
}

func TestClassifyCompletedIsConverged(t *testing.T) {
	p := NewPlanner(RecoverConfig{WorkspaceID: "ws"}, []RecoveryItem{
		{DocID: "doc-6", Status: domain.StatusCompleted},
	})
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
	})
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
	})
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
	p := NewPlanner(RecoverConfig{WorkspaceID: "ws"}, nil)
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
	p := NewPlanner(RecoverConfig{WorkspaceID: "ws"}, nil)
	plan := &RecoveryPlan{
		Items: []RecoveryItem{
			{DocID: "x", Classification: ClassTerminal, Action: ActionRefuse},
		},
	}
	if err := p.Validate(context.Background(), plan); err != nil {
		t.Errorf("unexpected validation error: %v", err)
	}
}

func TestValidateRefusesALiveLease(t *testing.T) {
	p := NewPlanner(RecoverConfig{WorkspaceID: "ws"}, nil)
	plan := &RecoveryPlan{
		Items: []RecoveryItem{{
			DocID: "x", Classification: ClassRetryable, Action: ActionRetryTransition,
			Jobs: []JobRef{{ID: "job-1", State: "RUNNING", Generation: 3, Live: true}},
		}},
	}
	if err := p.Validate(context.Background(), plan); err == nil {
		t.Error("expected validation error for a document whose job holds a live lease")
	}
}

func TestClassifyRefusesALiveLease(t *testing.T) {
	p := NewPlanner(RecoverConfig{WorkspaceID: "ws"}, []RecoveryItem{{
		DocID: "doc-live", Status: domain.StatusProcessing,
		Jobs: []JobRef{{ID: "job-1", State: "RUNNING", Generation: 2, Live: true}},
	}})
	item := p.Plan(context.Background()).Items[0]
	if item.Action != ActionRefuse || item.Classification != ClassNotRecoverable {
		t.Errorf("live job classified %q/%q, want refused/not_recoverable", item.Classification, item.Action)
	}
}

func TestExpiredLeaseDoesNotBlockRecovery(t *testing.T) {
	p := NewPlanner(RecoverConfig{WorkspaceID: "ws"}, []RecoveryItem{{
		DocID: "doc-dead", Status: domain.StatusProcessing,
		Jobs: []JobRef{{ID: "job-1", State: "RUNNING", Generation: 2, Live: false}},
	}})
	item := p.Plan(context.Background()).Items[0]
	if item.Classification != ClassRetryable {
		t.Errorf("expired lease classified %q, want retryable", item.Classification)
	}
}

func TestValidatePassesOnAnIdleQueue(t *testing.T) {
	p := NewPlanner(RecoverConfig{WorkspaceID: "ws"}, nil)
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
	})
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
