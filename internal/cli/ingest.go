package cli

import (
	"context"
	"fmt"
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

	// Probe the embedding endpoint while the stores are being connected. Its
	// first request on a fresh connection costs ~650ms even warm — the server
	// does per-connection setup, reproducible with curl — and much more when
	// it has been idle long enough to page its model back in. That is not our
	// cost to remove, but it is ours to stop paying sequentially: connecting
	// to RustFS, Postgres and NATS takes ~360ms and depends on none of it
	// (deviation 24).
	if err := cfg.RequireEmbedding(); err != nil {
		return err
	}
	embedder := embedding.New(cfg.Embedding)

	type probeResult struct {
		info embedding.ModelInfo
		took time.Duration
		err  error
	}
	probeCh := make(chan probeResult, 1)
	go func() {
		start := time.Now()
		i, err := embedder.Probe(ctx)
		probeCh <- probeResult{info: i, took: time.Since(start), err: err}
	}()

	needs := app.Needs{RustFS: true, Postgres: true, NATS: true, Metrics: true}
	// An earlier revision had --ingest-all call CreateWorkspace idempotently,
	// which was convenient and widened the most commonly run command to require
	// shared root credentials (deviation 10 recorded that as a real cost at the
	// time). There is no provisioning left to widen it: workspace creation is
	// `./pocket-advisor.sh deploy-workspaces`, run once by a human (deviation
	// 39), and every mode — ingest, query, mcp — connects with one
	// workspace's own credentials and nothing more (deviation 24).
	// --scan needs the uploader role as much as --ingest-all does: it triggers
	// work by touching raw/ objects, which only that role may do (§5.1).
	if o.IngestAll || o.Scan {
		needs.Uploader = true
	}

	a, err := app.New(ctx, cfg, logs, needs, o.WorkspaceID)
	if err != nil {
		return err
	}
	defer a.Close()

	// The one thing `./pocket-advisor.sh deploy-workspaces` cannot do — the
	// schema, since its vector width needs the embedding endpoint, reachable
	// only from this host (§4.4). Idempotent and cheap, so it runs on every
	// ingest rather than needing a separate provisioning command (deviation
	// 24, deviation 39).
	// Fatal at startup, not a warning: embedding at one dimension into a column
	// sized for another writes vectors that are silently not comparable to
	// their neighbours (§4.4).
	probeStart := time.Now()
	info, err := embedder.Probe(ctx)
	if err != nil {
		return fmt.Errorf("embedding endpoint %s: %w", cfg.Embedding.Endpoint, err)
	}
	probeTook := time.Since(probeStart)

	// Before VerifyDimension, which reads the schema this creates. Given the
	// probe rather than repeating it (deviation 24).
	if err := provision.EnsureWorkspace(ctx, cfg, o.WorkspaceID, info, a.Log); err != nil {
		return err
	}

	if err := a.DB.VerifyDimension(ctx, cfg.Embedding.Model, info.Dimension); err != nil {
		return fmt.Errorf("dimension check failed (endpoint reports %d for %s): %w",
			info.Dimension, cfg.Embedding.Model, err)
	}
	// probe_ms is here because it is the one startup cost that is not ours:
	// a warm endpoint answers in ~13ms, an idle one that has to page its model
	// back in can take tens of seconds, and the difference is invisible
	// otherwise — the process simply sits at 0% CPU before the dashboard
	// appears (deviation 24).
	a.Log.Info("embedding endpoint verified",
		"model", cfg.Embedding.Model, "dimension", info.Dimension,
		"sessions", embedder.Concurrency(), "probe_ms", probeTook.Milliseconds())

	// The BM25 lexical index is dropped before a full ingest run's writes
	// begin and rebuilt once after they all land (below, after WaitDrained) —
	// pg_textsearch's own guidance is to load data before indexing it, and
	// this run is about to stream chunk inserts continuously rather than in
	// one bulk load. --scan/--reconcile add only a handful of documents to an
	// already-indexed, already-queried workspace, so they leave the index in
	// place rather than paying for a full rebuild each time.
	//
	// Not fatal: an index that fails to drop degrades write throughput during
	// this run, it does not lose or corrupt anything, and aborting the whole
	// ingest over it would be a worse trade (§7 silent-degradation principle
	// aside — this is reported, just not fatal).
	if o.IngestAll {
		if err := a.DB.DropSearchIndex(ctx); err != nil {
			a.Log.Warn("search index drop failed; writes may be slower than usual this run",
				"error", err)
		}
	}

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

	// Built on ctx, not workCtx: whatever landed before an interrupt is still
	// real committed data and deserves to be searchable, even if the run that
	// wrote it was cut short.
	if o.IngestAll {
		if err := a.DB.BuildSearchIndex(ctx); err != nil {
			a.Log.Warn("search index build failed; lexical search will underperform until the next full ingest",
				"error", err)
		}
	}

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
