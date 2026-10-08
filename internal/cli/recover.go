package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/doctor"
	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/telemetry"
)

// recoverySelectionCap bounds how many documents each state contributes to one
// plan, so a plan stays reviewable. A selection that reaches it says so: more
// exist, and applying this plan and running --recover again selects the next
// batch.
const recoverySelectionCap = 500

// noteIfCapped tells the operator a selection stopped at the cap. It writes to
// stderr so --json output stays machine-readable.
func noteIfCapped(selected int, state string) {
	if selected >= recoverySelectionCap {
		fmt.Fprintf(os.Stderr, "note: %d %s documents selected, the per-run cap; more may exist — apply, then run --recover again\n",
			selected, state)
	}
}

// runRecover executes the --recover mode: plan and optionally apply recovery.
func runRecover(o *Options, cfg *config.Config, logs *telemetry.Logs) error {
	ctx := context.Background()
	log := logs.Logger(telemetry.RoleApp)

	// Connect to PostgreSQL
	dsn, err := cfg.WorkspacePostgresDSN(o.WorkspaceID)
	if err != nil {
		return fmt.Errorf("workspace %q: %w", o.WorkspaceID, err)
	}
	db, err := postgres.Connect(ctx, dsn, cfg.Postgres.MaxConns)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer db.Close()
	// Scope is asserted before any queue access.
	if err := db.AssertWorkspaceScope(ctx, o.WorkspaceID); err != nil {
		return err
	}
	unlock, err := db.LockMaintenance(ctx)
	if err != nil {
		return err
	}
	defer unlock()

	// The queue columns recovery reads and resets exist only after the
	// upgrade; refuse on the old layout rather than run a query it cannot answer.
	ready, err := db.QueueSchemaReady(ctx)
	if err != nil {
		return fmt.Errorf("check queue schema: %w", err)
	}
	if !ready {
		return fmt.Errorf("the job queue schema needs the upgrade first: stop any running " +
			"ingestion, then run --reconcile, which applies it under maintenance exclusion")
	}

	q := postgres.NewDoctorQueries(db)
	threshold := doctor.StalenessThreshold

	// Collect documents based on state filters
	var docs []doctor.RecoveryItem

	// Stale PENDING
	sp, err := q.StalePENDING(ctx, threshold, recoverySelectionCap)
	if err != nil {
		log.Warn("query stale pending failed", "error", err)
	}
	noteIfCapped(len(sp), "stale PENDING")
	for _, d := range sp {
		docs = append(docs, doctor.RecoveryItem{
			DocID:  d.DocID,
			Status: domain.StatusPending,
			Reason: domain.FailureReason(d.Reason),
		})
	}

	// Stale PROCESSING
	sproc, err := q.StalePROCESSING(ctx, threshold, recoverySelectionCap)
	if err != nil {
		log.Warn("query stale processing failed", "error", err)
	}
	noteIfCapped(len(sproc), "stale PROCESSING")
	for _, d := range sproc {
		docs = append(docs, doctor.RecoveryItem{
			DocID:  d.DocID,
			Status: domain.StatusProcessing,
			Reason: domain.FailureReason(d.Reason),
		})
	}

	// Retryable FAILED
	failed, err := q.RetryableFAILED(ctx, recoverySelectionCap)
	if err != nil {
		log.Warn("query retryable failed failed", "error", err)
	}
	noteIfCapped(len(failed), "retryable FAILED")
	for _, d := range failed {
		fr := domain.FailureReason(d.Reason)
		if domain.Retryable(fr) {
			docs = append(docs, doctor.RecoveryItem{
				DocID:  d.DocID,
				Status: domain.StatusFailed,
				Reason: fr,
			})
		}
	}

	// Attach each document's jobs, so the plan sees live leases and the
	// application step can revalidate exactly what was planned.
	ids := make([]string, 0, len(docs))
	for _, d := range docs {
		ids = append(ids, d.DocID)
	}
	jobs, err := q.JobsForDocuments(ctx, ids)
	if err != nil {
		return fmt.Errorf("read jobs: %w", err)
	}
	for i := range docs {
		for _, j := range jobs[docs[i].DocID] {
			docs[i].Jobs = append(docs[i].Jobs, doctor.JobRef{
				ID: j.ID, State: j.State, Generation: j.Generation, Attempts: j.Attempts, Live: j.Live,
			})
		}
	}

	planner := doctor.NewPlanner(doctor.RecoverConfig{
		WorkspaceID: o.WorkspaceID,
		Yes:         o.Yes,
	}, docs)

	plan := planner.Plan(ctx)
	validation := planner.Validate(ctx, plan)

	// Output
	if o.JSON {
		b, err := json.MarshalIndent(plan, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal plan: %w", err)
		}
		fmt.Println(string(b))
	} else {
		printPlan(plan)
	}

	// A validation failure stops application. A dry run still reports it.
	if validation != nil {
		if !plan.DryRun {
			return validation
		}
		fmt.Printf("\n%v\n", validation)
		return nil
	}

	// Execute if not dry-run
	if !plan.DryRun {
		return executeRecovery(ctx, q, plan, log)
	}

	return nil
}

// printPlan renders the plan in human-readable form.
func printPlan(plan *doctor.RecoveryPlan) {
	fmt.Printf("recovery plan for workspace %s\n", plan.WorkspaceID)
	if plan.DryRun {
		fmt.Println("  mode: DRY RUN (use --yes to apply)")
	} else {
		fmt.Println("  mode: APPLY")
	}
	fmt.Printf("  %s\n\n", plan.Summary)

	if len(plan.Items) == 0 {
		fmt.Println("  no items to recover")
		return
	}

	for _, item := range plan.Items {
		fmt.Printf("  %s [%s] %s\n", item.DocID, item.Classification, item.Action)
		if item.Reason != "" {
			fmt.Printf("    reason: %s\n", item.Reason)
		}
		if item.Requeue != "" {
			fmt.Printf("    → %s\n", item.Requeue)
		}
		if item.ObjectOperation != "" {
			fmt.Printf("    → %s\n", item.ObjectOperation)
		}
	}
}

// executeRecovery applies the plan by transitioning documents.
func executeRecovery(ctx context.Context, q *postgres.DoctorQueries, plan *doctor.RecoveryPlan, log *slog.Logger) error {
	var applied, failed, refused int

	for _, item := range plan.Items {
		if item.Action == doctor.ActionSkip || item.Action == doctor.ActionRefuse {
			continue
		}

		expected := make([]postgres.DoctorJob, 0, len(item.Jobs))
		for _, j := range item.Jobs {
			expected = append(expected, postgres.DoctorJob{ID: j.ID, State: j.State, Generation: j.Generation})
		}
		ok, err := q.ResetToPENDING(ctx, item.DocID, expected)
		if errors.Is(err, postgres.ErrJobLive) || errors.Is(err, postgres.ErrPlanChanged) {
			// Revalidation failed: nothing was changed for this document.
			log.Warn("recovery refused for document", "doc_id", item.DocID, "error", err)
			refused++
			continue
		}
		if err != nil {
			log.Error("reset to pending failed", "doc_id", item.DocID, "error", err)
			failed++
			continue
		}
		if ok {
			log.Info("reset to pending", "doc_id", item.DocID)
			applied++
		} else {
			log.Debug("document already in expected state", "doc_id", item.DocID)
		}
	}

	fmt.Printf("\napplied: %d, refused: %d, failed: %d, total: %d\n", applied, refused, failed, len(plan.Items))
	return nil
}
