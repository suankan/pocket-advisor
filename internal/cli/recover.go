package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/doctor"
	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/engine/email"
	"github.com/suankan/pocket-advisor/internal/source"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/telemetry"
	"github.com/suankan/pocket-advisor/internal/workspace"
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

	// Collect documents: those the operator named by reason, or by default
	// those whose state or reason makes them recoverable.
	var docs []doctor.RecoveryItem
	switch {
	case o.RecoverAll:
		docs = selectStale(ctx, q, doctor.StalenessThreshold, log)
		failed, err := selectByReasons(ctx, q, nil)
		if err != nil {
			return err
		}
		docs = append(docs, failed...)
	case len(o.RecoverReasons) > 0:
		docs, err = selectByReasons(ctx, q, o.RecoverReasons)
		if err != nil {
			return err
		}
	default:
		docs = selectStale(ctx, q, doctor.StalenessThreshold, log)
		docs = append(docs, selectRedrivable(ctx, q, log)...)
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
		AllFailed:   o.RecoverAll,
		FailureReas: o.RecoverReasons,
	}, docs)

	plan := planner.Plan(ctx)
	validation := planner.Validate(ctx, plan)
	read, closeOriginals := originals(o, cfg, db, log)
	defer closeOriginals()
	describeItems(ctx, q, plan, read, log)

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

// selectStale selects PENDING and PROCESSING documents older than the
// threshold. A query that fails is logged and its state left out.
func selectStale(ctx context.Context, q *postgres.DoctorQueries, threshold time.Duration, log *slog.Logger) []doctor.RecoveryItem {
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
	return docs
}

// selectRedrivable selects FAILED documents whose reason is retryable or a
// decline, or that have no job. A query that fails is logged.
func selectRedrivable(ctx context.Context, q *postgres.DoctorQueries, log *slog.Logger) []doctor.RecoveryItem {
	var docs []doctor.RecoveryItem
	failed, err := q.RedrivableFAILED(ctx, recoverySelectionCap)
	if err != nil {
		log.Warn("query redrivable failed failed", "error", err)
	}
	noteIfCapped(len(failed), "redrivable FAILED")
	for _, d := range failed {
		docs = append(docs, doctor.RecoveryItem{
			DocID:   d.DocID,
			Status:  domain.StatusFailed,
			Reason:  domain.FailureReason(d.Reason),
			Jobless: d.Jobless,
		})
	}
	return docs
}

// selectByReasons selects FAILED documents with the reasons the operator
// named, whatever their class, or every FAILED document when none is named.
func selectByReasons(ctx context.Context, q *postgres.DoctorQueries, reasons []domain.FailureReason) ([]doctor.RecoveryItem, error) {
	names := make([]string, 0, len(reasons))
	for _, r := range reasons {
		names = append(names, string(r))
	}
	failed, err := q.FAILEDWithReasons(ctx, names, recoverySelectionCap)
	if err != nil {
		return nil, err
	}
	noteIfCapped(len(failed), strings.TrimSpace("FAILED "+strings.Join(names, ",")))
	docs := make([]doctor.RecoveryItem, 0, len(failed))
	for _, d := range failed {
		docs = append(docs, doctor.RecoveryItem{
			DocID:   d.DocID,
			Status:  domain.StatusFailed,
			Reason:  domain.FailureReason(d.Reason),
			Jobless: d.Jobless,
		})
	}
	return docs, nil
}

// originalReader reads a document's original bytes back, verified by hash.
type originalReader func(ctx context.Context, ref, sha string) ([]byte, error)

// originals reads the workspace's configured originals, read-only, or returns
// nil when they cannot be opened: describing a plan never depends on them.
// The caller closes what it opened.
func originals(o *Options, cfg *config.Config, db *postgres.DB, log *slog.Logger) (originalReader, func()) {
	ws, err := workspace.Load(cfg.WorkspacesConfigPath, o.WorkspaceID)
	if err != nil {
		log.Warn("originals unavailable to describe recovery items", "error", err)
		return nil, func() {}
	}
	r, err := source.New(ws.AbsPaths, ws.Ignore.Match, postgres.NewSourceRepo(db))
	if err != nil {
		log.Warn("originals unavailable to describe recovery items", "error", err)
		return nil, func() {}
	}
	return r.Read, r.Close
}

// describeItems says where each planned document is, so the operator can
// find the file behind it. A description that cannot be read leaves the plan
// as it is.
func describeItems(ctx context.Context, q *postgres.DoctorQueries, plan *doctor.RecoveryPlan, read originalReader, log *slog.Logger) {
	ids := make([]string, 0, len(plan.Items))
	for _, item := range plan.Items {
		ids = append(ids, item.DocID)
	}
	described, err := q.Describe(ctx, ids)
	if err != nil {
		log.Warn("describing recovery items failed", "error", err)
		return
	}
	applyDescriptions(ctx, plan, described, read)
}

// applyDescriptions sets each item's location and email. An email whose
// header was never stored, because its body could not be read, is named
// from its original's header when read is set.
func applyDescriptions(ctx context.Context, plan *doctor.RecoveryPlan, described map[string]postgres.Description, read originalReader) {
	for i, item := range plan.Items {
		d, ok := described[item.DocID]
		if !ok {
			continue
		}
		plan.Items[i].Location = d.Location
		if d.From == "" && d.Subject == "" && d.DocType == "email" && read != nil {
			if data, err := read(ctx, source.Ref(item.DocID), d.SHA256); err == nil {
				if s, err := email.Summarize(data); err == nil {
					d.From, d.Subject, d.Date = s.From, s.Subject, s.Date
				}
			}
		}
		if d.From != "" || d.Subject != "" {
			e := &doctor.EmailRef{From: d.From, Subject: d.Subject, Container: d.InEmail}
			if !d.Date.IsZero() {
				e.Date = d.Date.Format(time.DateOnly)
			}
			plan.Items[i].Email = e
		}
	}
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
		if item.Location != "" {
			fmt.Printf("    file: %s\n", item.Location)
		}
		if e := item.Email; e != nil {
			label := "email"
			if e.Container {
				label = "in email"
			}
			fmt.Printf("    %s: %s\n", label, emailLine(e))
		}
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

// emailLine names a message as "date, from sender: subject", leaving out what
// it lacks.
func emailLine(e *doctor.EmailRef) string {
	var parts []string
	if e.Date != "" {
		parts = append(parts, e.Date)
	}
	if e.From != "" {
		parts = append(parts, "from "+e.From)
	}
	line := strings.Join(parts, ", ")
	if e.Subject != "" {
		if line != "" {
			line += ": "
		}
		line += strings.TrimSpace(e.Subject)
	}
	return line
}
