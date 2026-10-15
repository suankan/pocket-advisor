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

// JobRef is the planner's view of one unfinished or failed job of a document.
// Application revalidates it against durable state, so a plan made before the
// state moved cannot reset work it never saw.
type JobRef struct {
	ID         string `json:"job_id"`
	State      string `json:"state"`
	Generation int64  `json:"generation"`
	Attempts   int    `json:"attempts"`
	// Live is a RUNNING job under an unexpired lease: a handler may still be
	// working on it.
	Live bool `json:"live,omitempty"`
}

// RecoveryItem is one document the planner has classified.
type RecoveryItem struct {
	DocID           string                 `json:"doc_id"`
	Status          domain.Status          `json:"status"`
	Reason          domain.FailureReason   `json:"reason,omitempty"`
	Jobs            []JobRef               `json:"jobs,omitempty"`
	Classification  RecoveryClassification `json:"classification"`
	Action          RecoveryAction         `json:"action"`
	ObjectOperation string                 `json:"object_operation,omitempty"`
	Requeue         string                 `json:"requeue,omitempty"`
}

// RecoveryPlan is the complete output of the planner.
type RecoveryPlan struct {
	WorkspaceID string         `json:"workspace_id"`
	DryRun      bool           `json:"dry_run"`
	Items       []RecoveryItem `json:"items"`
	Summary     string         `json:"summary"`
}

// RecoverConfig controls what the planner selects and how it classifies.
type RecoverConfig struct {
	// Filters
	States      []domain.Status
	MinAge      time.Duration
	FailureReas []domain.FailureReason
	DocIDs      []string

	// Scope
	WorkspaceID string
	Yes         bool // --yes to allow mutation
}

// RecoveryPlanner produces a plan for the given documents. It never
// writes to stores; it only classifies and recommends.
type RecoveryPlanner struct {
	cfg       RecoverConfig
	documents []RecoveryItem
}

// NewPlanner creates a RecoveryPlanner. The documents slice is the set
// of items the caller has already queried from the database, each with the
// jobs the document currently has.
func NewPlanner(cfg RecoverConfig, docs []RecoveryItem) *RecoveryPlanner {
	return &RecoveryPlanner{cfg: cfg, documents: docs}
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

	// A job under an unexpired lease means a handler may still be working on
	// this document. Resetting it now would race that handler, so it is refused
	// until the lease ends or the work finishes.
	for _, j := range doc.Jobs {
		if j.Live {
			item.Classification = ClassNotRecoverable
			item.Action = ActionRefuse
			return item
		}
	}

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
		item.Requeue = "return the document's failed and expired jobs to the queue with a fresh retry cycle; queued and source-waiting jobs are left as they are"
		return item
	}

	// Stale PROCESSING: the original handler is gone. Reset to pending.
	if doc.Status == domain.StatusProcessing {
		item.Classification = ClassRetryable
		item.Action = ActionResetProcessing
		item.Requeue = "return the document's failed and expired jobs to the queue with a fresh retry cycle; queued and source-waiting jobs are left as they are"
		return item
	}

	// FAILED: classify by reason.
	if doc.Status == domain.StatusFailed {
		switch domain.ClassifyReason(doc.Reason) {
		case domain.ClassRetryable:
			item.Classification = ClassRetryable
			item.Action = ActionRetryTransition
			item.Requeue = "return the document's failed and expired jobs to the queue with a fresh retry cycle; queued and source-waiting jobs are left as they are"
		case domain.ClassTerminal:
			item.Classification = ClassTerminal
			item.Action = ActionRefuse
		case domain.ClassExpectedDecline:
			// A decline is recorded SKIPPED. Recorded as a failure, it came from
			// a handler that predates the decline; running it again settles it.
			item.Classification = ClassRetryable
			item.Action = ActionRetryTransition
			item.Requeue = "run the document again so the current handler records the decline it failed under; its failed jobs return to the queue with a fresh retry cycle"
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

// Validate checks the plan for safety before execution. It refuses broad
// redrive of terminal failures and any redrive of a document whose job holds
// a live lease. A validation error stops application.
func (p *RecoveryPlanner) Validate(ctx context.Context, plan *RecoveryPlan) error {
	var issues []string

	for _, item := range plan.Items {
		if item.Classification == ClassTerminal && item.Action != ActionRefuse {
			issues = append(issues,
				fmt.Sprintf("refusing broad redrive of terminal failure: %s (%s)",
					item.DocID, item.Reason))
		}
		if item.Action != ActionRefuse && item.Action != ActionSkip {
			for _, j := range item.Jobs {
				if j.Live {
					issues = append(issues,
						fmt.Sprintf("refusing to reset %s: job %s holds a live lease", item.DocID, j.ID))
				}
			}
		}
	}

	if len(issues) > 0 {
		return fmt.Errorf("plan validation failed:\n  %s", strings.Join(issues, "\n  "))
	}
	return nil
}
