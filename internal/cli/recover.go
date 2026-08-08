package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/doctor"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/telemetry"
)

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

	q := postgres.NewDoctorQueries(db)
	threshold := doctor.StalenessThreshold

	// Collect documents based on state filters
	var docs []doctor.RecoveryItem

	// Stale PENDING
	sp, err := q.StalePENDING(ctx, threshold, 500)
	if err != nil {
		log.Warn("query stale pending failed", "error", err)
	}
	for _, d := range sp {
		docs = append(docs, doctor.RecoveryItem{
			DocID:  d.DocID,
			Status: domain.StatusPending,
			Reason: domain.FailureReason(d.Reason),
		})
	}

	// Stale PROCESSING
	sproc, err := q.StalePROCESSING(ctx, threshold, 500)
	if err != nil {
		log.Warn("query stale processing failed", "error", err)
	}
	for _, d := range sproc {
		docs = append(docs, doctor.RecoveryItem{
			DocID:  d.DocID,
			Status: domain.StatusProcessing,
			Reason: domain.FailureReason(d.Reason),
		})
	}

	// Retryable FAILED
	failed, err := q.RetryableFAILED(ctx, 500)
	if err != nil {
		log.Warn("query retryable failed failed", "error", err)
	}
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

	// Create and run the planner
	planner := doctor.NewPlanner(doctor.RecoverConfig{
		WorkspaceID: o.WorkspaceID,
		Yes:         o.Yes,
	}, docs, nil) // nil bus — no active stream check in basic mode

	plan := planner.Plan(ctx)

	// Validate the plan
	if err := planner.Validate(ctx, plan); err != nil {
		log.Warn("plan validation", "error", err)
	}

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
		if item.MessagePublish != "" {
			fmt.Printf("    → %s\n", item.MessagePublish)
		}
		if item.ObjectOperation != "" {
			fmt.Printf("    → %s\n", item.ObjectOperation)
		}
	}
}

// executeRecovery applies the plan by transitioning documents.
func executeRecovery(ctx context.Context, q *postgres.DoctorQueries, plan *doctor.RecoveryPlan, log *slog.Logger) error {
	var applied, failed int

	for _, item := range plan.Items {
		if item.Action == doctor.ActionSkip || item.Action == doctor.ActionRefuse {
			continue
		}

		ok, err := q.ResetToPENDING(ctx, item.DocID)
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

	fmt.Printf("\napplied: %d, failed: %d, total: %d\n", applied, failed, len(plan.Items))
	return nil
}
