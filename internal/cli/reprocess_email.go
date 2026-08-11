package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/suankan/pocket-advisor/internal/app"
	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/telemetry"
	"github.com/suankan/pocket-advisor/internal/worker"
)

// runReprocessEmail executes --reprocess-email-metadata: rebuild the durable
// email browse metadata of one workspace from its authoritative Tier 1 bytes
// (ingestion-design.md §2.5).
//
// This is the supported path for a workspace ingested before those tables
// existed. The schema upgrade adds the tables and deliberately synthesises
// nothing; only the message bytes can say who wrote what, to whom, and in
// answer to which message, so this command goes back to them.
//
// It is not an ingest. Nothing is uploaded, enqueued, extracted, embedded or
// re-chunked; documents, chunks, thread ids and processing statuses are left
// exactly as they are. The only writes are the email metadata tables, through
// the same idempotent transaction the email worker uses, so a run that is
// interrupted, repeated, or overlapped with a live ingest converges instead of
// duplicating rows or moving a browse cursor's watermark.
func runReprocessEmail(o *Options, cfg *config.Config, logs *telemetry.Logs) error {
	// A corpus-sized walk deserves the same courtesy an ingest gets: the
	// signal cancels the run, the batch in flight stops at its next boundary,
	// and the summary still reports what was rebuilt before the interrupt.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Tier 1 as the worker role — this reads objects and must never be able to
	// write under raw/ — and Postgres for selection and the metadata write. No
	// NATS: nothing is published, because nothing is being re-extracted.
	a, err := app.New(ctx, cfg, logs, app.Needs{RustFS: true, Postgres: true}, o.WorkspaceID)
	if err != nil {
		return err
	}
	defer a.Close()

	log := a.Logger(telemetry.RoleEmail)
	r := &worker.EmailMetadataReprocessor{
		Docs:   a.Emails,
		Vault:  a.Vault,
		Emails: a.Emails,
		Log:    log,
	}

	opts := worker.EmailReprocessOptions{
		WorkspaceID: o.WorkspaceID,
		Limit:       o.ReprocessLimit,
		Concurrency: o.ReprocessConc,
		OnlyMissing: o.ReprocessMissing,
		DryRun:      o.DryRun,
	}
	if !o.JSON {
		last := time.Now()
		opts.Progress = func(s worker.EmailReprocessSummary) {
			// Batch-paced rather than per-document, and throttled, because the
			// terminal is not the point of the run and a progress line per
			// message would bury the summary.
			if time.Since(last) < progressInterval {
				return
			}
			last = time.Now()
			fmt.Printf("  reprocessed %d (updated %d, unreadable %d, failed %d)\n",
				s.Processed, s.Updated, s.Unreadable, s.Failed)
		}
	}

	log.Info("email metadata reprocessing started",
		"workspace_id", o.WorkspaceID, "limit", opts.Limit,
		"only_missing", opts.OnlyMissing, "dry_run", opts.DryRun)

	summary, runErr := r.Run(ctx, opts)

	log.Info("email metadata reprocessing finished",
		"workspace_id", summary.WorkspaceID, "processed", summary.Processed,
		"updated", summary.Updated, "unreadable", summary.Unreadable,
		"failed", summary.Failed, "dry_run", summary.DryRun)

	if o.JSON {
		b, err := json.MarshalIndent(summary, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal summary: %w", err)
		}
		fmt.Println(string(b))
	} else {
		printReprocessSummary(summary)
	}

	if runErr != nil {
		return runErr
	}
	// Unreadable documents are the reason this command reports rather than
	// repairs: their metadata is still missing, and nothing here can invent
	// it. Failures are the same story from the other end — bytes that were
	// read and could not be understood. Either way the run did not finish the
	// job it was given, and must not claim it did.
	if summary.Unreadable > 0 || summary.Failed > 0 {
		return fmt.Errorf("%d document(s) unreadable and %d failed; see %s",
			summary.Unreadable, summary.Failed, logs.Dir())
	}
	return nil
}

// progressInterval throttles the progress line.
const progressInterval = 2 * time.Second

// printReprocessSummary renders the counts in human-readable form.
func printReprocessSummary(s worker.EmailReprocessSummary) {
	mode := "APPLY"
	if s.DryRun {
		mode = "DRY RUN (no metadata written)"
	}
	fmt.Printf("\nemail metadata reprocessing for workspace %s\n", s.WorkspaceID)
	fmt.Printf("  mode: %s\n", mode)
	fmt.Printf("  processed:  %d\n", s.Processed)
	fmt.Printf("  updated:    %d\n", s.Updated)
	fmt.Printf("  unreadable: %d\n", s.Unreadable)
	fmt.Printf("  failed:     %d\n", s.Failed)
	for _, code := range reprocessReasonOrder {
		if n := s.Reasons[code]; n > 0 {
			fmt.Printf("    %-20s %d\n", code, n)
		}
	}
}

// reprocessReasonOrder fixes the reporting order of the closed reason set, so
// two runs of the same corpus print the same lines in the same places.
var reprocessReasonOrder = []string{
	worker.ReasonReprocessNoObject,
	worker.ReasonReprocessBadObjectURI,
	worker.ReasonReprocessUnreadable,
	worker.ReasonReprocessUnknownEncode,
	worker.ReasonReprocessParseFailed,
	worker.ReasonReprocessWriteFailed,
	worker.ReasonReprocessUnclassified,
}
