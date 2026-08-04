package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/suankan/pocket-advisor/internal/app"
	"github.com/suankan/pocket-advisor/internal/client/embedding"
	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/dashboard"
	"github.com/suankan/pocket-advisor/internal/discovery"
	"github.com/suankan/pocket-advisor/internal/pipeline"
	"github.com/suankan/pocket-advisor/internal/provision"
	"github.com/suankan/pocket-advisor/internal/telemetry"
	"github.com/suankan/pocket-advisor/internal/uploader"
	"github.com/suankan/pocket-advisor/internal/workspace"
)

// settle is how long the pipeline must stay idle before a run is considered
// finished. Finishing an email publishes attachment work that has not reached
// its queue yet, so the pipeline passes through empty on its way to being busy.
const settle = 3 * time.Second

// runIngest covers the three modes that enqueue work: --ingest-all, --scan and
// --reconcile. They differ only in how work reaches the queues; all three then
// run the pools until everything drains.
func runIngest(o *Options, cfg *config.Config, logs *telemetry.Logs) error {
	ctx := context.Background()

	needs := app.Needs{RustFS: true, Postgres: true, NATS: true, Metrics: true}
	// Deliberately does NOT provision. An earlier revision had --ingest-all
	// call CreateWorkspace idempotently, which was convenient and widened the
	// most commonly run command to require shared root credentials
	// (deviation 10 recorded that as a real cost at the time). Provisioning is
	// now --create-workspace's job alone, so everything else — ingest, query,
	// mcp — connects with one workspace's own credentials and nothing more.
	// --scan needs it as much as --ingest-all does: it triggers work by
	// touching raw/ objects, which only the uploader role may do (§5.1).
	if o.IngestAll || o.Scan {
		needs.Uploader = true
	}

	a, err := app.New(ctx, cfg, logs, needs, o.WorkspaceID)
	if err != nil {
		return err
	}
	defer a.Close()

	if err := checkNotifyTarget(ctx, cfg, o.WorkspaceID, a.Log); err != nil {
		return err
	}

	if err := cfg.RequireEmbedding(); err != nil {
		return err
	}
	embedder := embedding.New(cfg.Embedding)

	// Fatal at startup, not a warning: embedding at one dimension into a column
	// sized for another writes vectors that are silently not comparable to
	// their neighbours (§4.4).
	info, err := embedder.Probe(ctx)
	if err != nil {
		return fmt.Errorf("embedding endpoint %s: %w", cfg.Embedding.Endpoint, err)
	}
	if err := a.DB.VerifyDimension(ctx, cfg.Embedding.Model, info.Dimension); err != nil {
		return fmt.Errorf("dimension check failed (endpoint reports %d for %s): %w",
			info.Dimension, cfg.Embedding.Model, err)
	}
	a.Log.Info("embedding endpoint verified",
		"model", cfg.Embedding.Model, "dimension", info.Dimension,
		"sessions", embedder.Concurrency())

	stats := telemetry.NewStats()
	pipe, err := pipeline.New(ctx, a, stats, embedder,
		pipeline.Options{OCRLangs: o.OCRLangs, RustFSEvents: true, WorkspaceID: o.WorkspaceID})
	if err != nil {
		return err
	}
	defer pipe.Close()

	fetchCtx, workCtx, stopSignals := interrupts(ctx, a.Log)
	defer stopSignals()

	// The pools come up before any work is enqueued, so the first document is
	// being extracted while the rest are still being discovered.
	pipe.Start(fetchCtx, workCtx)

	view := dashboard.New(dashboard.Source{
		Stats:     stats,
		CPU:       a.CPU,
		Embedder:  embedder,
		DB:        a.DB,
		Mode:      o.Mode(),
		Workspace: o.WorkspaceID,
		LogDir:    logs.Dir(),
	}, os.Stdout)

	viewCtx, stopView := context.WithCancel(ctx)
	viewDone := make(chan struct{})
	if !o.NoDashboard {
		go func() {
			defer close(viewDone)
			view.Run(viewCtx)
		}()
	} else {
		close(viewDone)
	}

	feedErr := feed(fetchCtx, o, a, stats)

	// Drain even when feeding failed partway: work already enqueued is still
	// owned by this process, and abandoning it mid-flight would leave documents
	// PENDING with their messages unacked.
	drainErr := pipe.WaitDrained(workCtx, settle)

	stopSignals()
	pipe.Wait()

	stopView()
	<-viewDone

	fmt.Print(view.Summary())
	fmt.Printf("logs      %s\n", logs.Dir())

	if feedErr != nil {
		return feedErr
	}
	if drainErr != nil && workCtx.Err() == nil {
		return drainErr
	}
	if n := stats.DeadLettered(); n > 0 {
		// A run that finished but lost documents must not report success — the
		// whole point of the DLQ split is that these are failures, not skips.
		return fmt.Errorf("%d document(s) dead-lettered; see %s and the %s stream",
			n, logs.Dir(), "INGESTION_DLQ")
	}
	return nil
}

// feed puts work on the queues, by whichever route the mode selected.
func feed(ctx context.Context, o *Options, a *app.App, stats *telemetry.Stats) error {
	svc := &discovery.Service{
		Vault: a.Vault, Uploads: a.Uploads, Docs: a.Docs, Bus: a.Bus,
		Log: a.Logger(telemetry.RoleDiscover),
	}

	if o.IngestAll {
		if err := upload(ctx, o, a, stats); err != nil {
			return err
		}
	}

	if o.Reconcile {
		return reconcile(ctx, o, a, svc)
	}

	// --ingest-all and --scan both end here. Scanning rather than reacting to
	// bucket notifications is what makes an interrupted run resumable: it
	// compares Tier 1 against Tier 2 and enqueues the difference, so whatever
	// the previous run did not finish is simply still missing.
	n, err := svc.Scan(ctx, o.WorkspaceID, o.HighWater, o.LowWater)
	if err != nil {
		return fmt.Errorf("scan: %w", err)
	}
	a.Log.Info("scan complete", "enqueued", n)
	return nil
}

func upload(ctx context.Context, o *Options, a *app.App, stats *telemetry.Stats) error {
	ws, err := workspace.Load(o.WorkspaceConfig, o.WorkspaceID)
	if err != nil {
		return err
	}

	log := a.Logger(telemetry.RoleUploader)
	log.Info("workspace resolved",
		"workspace_id", ws.ID, "title", ws.Title, "collections", len(ws.Collections))

	up := uploader.New(a.Uploads, log)
	res, err := up.Run(ctx, uploader.Options{
		WorkspaceID: ws.ID,
		Collections: ws.Collections,
		Concurrency: uploadConcurrency,
		DryRun:      o.DryRun,
		RunID:       time.Now().UTC().Format("20060102T150405Z"),
		Progress:    &stats.Upload,
	})
	if err != nil {
		return err
	}
	log.Info("upload complete",
		"uploaded", res.Uploaded, "duplicate", res.Duplicate,
		"failed", res.Failed, "bytes", res.Bytes)

	if res.Failed > 0 {
		return fmt.Errorf("%d file(s) failed to upload", res.Failed)
	}
	return nil
}

// uploadConcurrency is I/O against one object store; the CPU count is not the
// relevant bound and neither is a knob worth having.
const uploadConcurrency = 8

// reconcile re-publishes documents whose stub committed but whose command never
// reached the broker.
//
// Safe to repeat: doc_id is deterministic and every worker is idempotent on it,
// so a duplicate delivery redoes work rather than creating a second document.
func reconcile(ctx context.Context, o *Options, a *app.App, svc *discovery.Service) error {
	docs, err := a.Docs.ClaimStalePending(ctx, o.StaleAfter, 500)
	if err != nil {
		return err
	}
	telemetry.DiscoveryStalePending.Set(float64(len(docs)))
	a.Log.Info("stale pending found", "count", len(docs))

	n := 0
	for _, d := range docs {
		key, err := a.Vault.KeyFromURI(d.RawURI)
		if err != nil {
			a.Log.Error("bad raw uri", "doc_id", d.DocID, "uri", d.RawURI, "error", err)
			continue
		}
		if err := svc.Ingest(ctx, d.WorkspaceID, key, "reconcile"); err != nil {
			a.Log.Error("republish failed", "doc_id", d.DocID, "error", err)
			continue
		}
		n++
	}
	remaining, _ := a.Docs.CountStalePending(ctx, o.StaleAfter)
	telemetry.DiscoveryStalePending.Set(float64(remaining))
	a.Log.Info("reconcile complete", "republished", n, "remaining", remaining)
	return nil
}

// checkNotifyTarget refuses to ingest while the notify target is aimed at a
// different workspace.
//
// RustFS has a single server-wide target, so the last --create-workspace wins.
// Ingesting anyway is not merely ineffective, it breaks isolation: the bucket
// rule fires for *this* workspace's objects while the target authenticates as
// the other one, so events naming this workspace's object keys are delivered
// into another workspace's NATS account. Measured, not theorised — a run of 79
// objects put all 79 into the wrong account before this check existed.
//
// Fatal rather than a warning for that reason. A warning was the first
// version, and it is what let those 79 through: the run reported success, the
// queues stayed at zero, and the leak was visible only by inspecting another
// workspace's streams.
//
// Being unable to check is treated differently from a known mismatch. Reading
// this needs cluster access that ingestion otherwise does not require, so an
// unreadable target degrades to a warning instead of blocking the command.
func checkNotifyTarget(ctx context.Context, cfg *config.Config, id string, log *slog.Logger) error {
	active, err := provision.ActiveNotifyWorkspace(ctx, cfg)
	switch {
	case err != nil:
		log.Warn("could not verify the rustfs notify target; "+
			"proceeding, but uploads may not trigger processing",
			"workspace_id", id, "error", err)
		return nil
	case active == id:
		return nil
	case active == "":
		log.Warn("rustfs notify target is not configured; "+
			"uploads will not trigger processing until --create-workspace has run",
			"workspace_id", id)
		return nil
	}
	return fmt.Errorf("rustfs notify target points at workspace %q, not %q: "+
		"ingesting now would deliver this workspace's events into %q's NATS account. "+
		"Run --create-workspace --workspace-id %s first",
		active, id, active, id)
}
