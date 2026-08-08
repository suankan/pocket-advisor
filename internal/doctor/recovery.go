package doctor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/suankan/pocket-advisor/internal/domain"
)

// RecoveryAction is what the planner intends to do with one document.
type RecoveryAction string

const (
	ActionRetryTransition RecoveryAction = "transition_to_pending"
	ActionResetProcessing RecoveryAction = "reset_to_pending"
	ActionRebuildBM25     RecoveryAction = "rebuild_bm25_index"
	ActionSkip            RecoveryAction = "no_action_needed"
	ActionConvergeReset   RecoveryAction = "converge_incomplete_reset"
	ActionRefuse          RecoveryAction = "refused"
)

// RecoveryClassification is whether an item is safe to redrive.
type RecoveryClassification string

const (
	ClassRetryable      RecoveryClassification = "retryable"
	ClassTerminal       RecoveryClassification = "terminal"
	ClassConverged      RecoveryClassification = "converged"
	ClassNotRecoverable RecoveryClassification = "not_recoverable"
)

// RecoveryItem is one document the planner has classified.
type RecoveryItem struct {
	DocID           string                `json:"doc_id"`
	Status          domain.Status         `json:"status"`
	Reason          domain.FailureReason  `json:"reason,omitempty"`
	Classification  RecoveryClassification `json:"classification"`
	Action          RecoveryAction        `json:"action"`
	ObjectOperation string                `json:"object_operation,omitempty"`
	MessagePublish  string                `json:"message_publish,omitempty"`
}

// RecoveryPlan is the complete output of the planner.
type RecoveryPlan struct {
	WorkspaceID string        `json:"workspace_id"`
	DryRun      bool          `json:"dry_run"`
	Items       []RecoveryItem `json:"items"`
	Summary     string        `json:"summary"`
}

// RecoverConfig controls what the planner selects and how it classifies.
type RecoverConfig struct {
	// Filters
	States       []domain.Status
	MinAge       time.Duration
	FailureReas  []domain.FailureReason
	DocIDs       []string

	// Scope
	WorkspaceID string
	Yes         bool // --yes to allow mutation
}

// RecoveryPlanner produces a plan for the given documents. It never
// writes to stores; it only classifies and recommends.
type RecoveryPlanner struct {
	cfg      RecoverConfig
	documents []RecoveryItem
	bus      RecoveryBus
}

// RecoveryBus is the JetStream subset the planner needs to check for
// active work, so it avoids republishing into an active consumer.
type RecoveryBus interface {
	// StreamInfo returns message counts for a stream, or an error if the
	// stream does not exist.
	StreamInfo(ctx context.Context, stream string) (msgs uint64, err error)
}

// NewPlanner creates a RecoveryPlanner. The documents slice is the set
// of items the caller has already queried from the database.
func NewPlanner(cfg RecoverConfig, docs []RecoveryItem, bus RecoveryBus) *RecoveryPlanner {
	return &RecoveryPlanner{cfg: cfg, documents: docs, bus: bus}
}

// Plan classifies every item and returns the recovery plan. It is
// read-only with respect to stores.
func (p *RecoveryPlanner) Plan(ctx context.Context) *RecoveryPlan {
	plan := &RecoveryPlan{
		WorkspaceID: p.cfg.WorkspaceID,
		DryRun:      !p.cfg.Yes,
	}

	var retryable, terminal, converged, refused int

	for _, doc := range p.documents {
		item := p.classify(doc)
		plan.Items = append(plan.Items, item)

		switch item.Classification {
		case ClassRetryable:
			retryable++
		case ClassTerminal:
			terminal++
		case ClassConverged:
			converged++
		case ClassNotRecoverable:
			refused++
		}
	}

	plan.Summary = fmt.Sprintf(
		"%d items: %d retryable, %d terminal, %d converged, %d not recoverable",
		len(plan.Items), retryable, terminal, converged, refused)
	return plan
}

// classify determines the action for one document.
func (p *RecoveryPlanner) classify(doc RecoveryItem) RecoveryItem {
	item := doc

	// Already converged: nothing to do.
	if doc.Status == domain.StatusCompleted {
		item.Classification = ClassConverged
		item.Action = ActionSkip
		return item
	}

	// SKIPPED is an expected decline — never recovered.
	if doc.Status == domain.StatusSkipped {
		item.Classification = ClassConverged
		item.Action = ActionSkip
		return item
	}

	// Stale PENDING: retryable, transition back to pending for republish.
	if doc.Status == domain.StatusPending {
		item.Classification = ClassRetryable
		item.Action = ActionRetryTransition
		item.MessagePublish = "re-publish to ingestion stream"
		return item
	}

	// Stale PROCESSING: the original handler is gone. Reset to pending.
	if doc.Status == domain.StatusProcessing {
		item.Classification = ClassRetryable
		item.Action = ActionResetProcessing
		item.MessagePublish = "re-publish to ingestion stream"
		return item
	}

	// FAILED: classify by reason.
	if doc.Status == domain.StatusFailed {
		switch domain.ClassifyReason(doc.Reason) {
		case domain.ClassRetryable:
			item.Classification = ClassRetryable
			item.Action = ActionRetryTransition
			item.MessagePublish = "re-publish to ingestion stream"
		case domain.ClassTerminal:
			item.Classification = ClassTerminal
			item.Action = ActionRefuse
		case domain.ClassExpectedDecline:
			item.Classification = ClassConverged
			item.Action = ActionSkip
		default:
			item.Classification = ClassNotRecoverable
			item.Action = ActionRefuse
		}
		return item
	}

	item.Classification = ClassNotRecoverable
	item.Action = ActionRefuse
	return item
}

// Validate checks the plan for safety before execution. It refuses
// broad redrive of terminal failures and flags active JetStream work.
func (p *RecoveryPlanner) Validate(ctx context.Context, plan *RecoveryPlan) error {
	var issues []string

	for _, item := range plan.Items {
		if item.Classification == ClassTerminal && item.Action != ActionRefuse {
			issues = append(issues,
				fmt.Sprintf("refusing broad redrive of terminal failure: %s (%s)",
					item.DocID, item.Reason))
		}
	}

	// Check that we are not republishing into an active stream with
	// significant pending work.
	if p.bus != nil {
		msgs, err := p.bus.StreamInfo(ctx, "INGESTION")
		if err == nil && msgs > 1000 {
			issues = append(issues,
				fmt.Sprintf("INGESTION stream has %d pending messages — "+
					"recovery may conflict with active ingestion work", msgs))
		}
	}

	if len(issues) > 0 {
		return fmt.Errorf("plan validation failed:\n  %s", strings.Join(issues, "\n  "))
	}
	return nil
}
