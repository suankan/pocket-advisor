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
	"github.com/suankan/pocket-advisor/internal/source"
	"github.com/suankan/pocket-advisor/internal/telemetry"
	"github.com/suankan/pocket-advisor/internal/workspace"
)

const settle = 3 * time.Second

func discoveryService(a *app.App) *discovery.Service {
	return &discovery.Service{DB: a.DB, Sources: a.Sources, Inventory: a.Inventory, Docs: a.Docs, Jobs: a.Jobs, Log: a.Logger(telemetry.RoleDiscover)}
}
func previewSources(ctx context.Context, o *Options) error {
	ws, err := workspace.Load(o.WorkspaceConfig, o.WorkspaceID)
	if err != nil {
		return err
	}
	sources, err := source.New(ws.AbsPaths, ws.Ignore.Match, nil)
	if err != nil {
		return err
	}
	defer sources.Close()
	count := 0
	var size int64
	err = sources.Walk(ctx, func(f source.File, _ []byte) error { count++; size += f.Size; return nil })
	fmt.Printf("source preview: files=%d bytes=%d (read-only; no database or queue changes)\n", count, size)
	return err
}
func runIngest(o *Options, cfg *config.Config, logs *telemetry.Logs) error {
	ctx := context.Background()
	if o.DryRun {
		return previewSources(ctx, o)
	}
	if err := cfg.RequireEmbedding(); err != nil {
		return err
	}
	a, err := app.New(ctx, cfg, logs, app.Needs{Sources: true, Postgres: true, NATS: true, Metrics: true}, o.WorkspaceID)
	if err != nil {
		return err
	}
	defer a.Close()
	unlock, err := a.DB.LockMaintenance(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	embedder := embedding.New(cfg.Embedding)
	info, err := embedder.Probe(ctx)
	if err != nil {
		return err
	}
	if err := provision.EnsureWorkspace(ctx, cfg, o.WorkspaceID, info, a.Log); err != nil {
		return err
	}
	if err := a.DB.VerifyDimension(ctx, cfg.Embedding.Model, info.Dimension); err != nil {
		return err
	}
	fetchCtx, workCtx, stopSignals := interrupts(ctx, a.Log)
	defer stopSignals()
	svc := discoveryService(a)
	// Binding is separate from intake: destructive planning sees complete
	// recorded containment and no worker can write children during the sweep.
	if err := svc.BindExisting(fetchCtx); err != nil {
		return err
	}
	if o.IngestAll && !o.NoPrune {
		if err := prune(fetchCtx, o, a); err != nil {
			return err
		}
	}
	// A stopped prior process owns no active jobs because of the workspace lock.
	if err := a.Jobs.Reconcile(fetchCtx, 0); err != nil {
		return err
	}
	stats := telemetry.NewStats()
	svc.Progress = &stats.Scan
	pipe, err := pipeline.New(ctx, a, stats, embedder, pipeline.Options{OCRLangs: o.OCRLangs})
	if err != nil {
		return err
	}
	defer pipe.Close()
	pipe.Start(fetchCtx, workCtx)
	view := dashboard.New(dashboard.Source{Stats: stats, CPU: a.CPU, Embedder: embedder, DB: a.DB, Mode: o.Mode(), Workspace: o.WorkspaceID, LogDir: logs.Dir()}, os.Stdout)
	viewCtx, stopView := context.WithCancel(ctx)
	defer stopView()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if !o.NoDashboard {
			view.Run(viewCtx)
		}
	}()
	var feedErr error
	_, feedErr = svc.Scan(fetchCtx, o.HighWater, o.LowWater)
	if feedErr == nil {
		feedErr = svc.ResumeLegacy(fetchCtx)
	}
	if o.Listen {
		ticker := time.NewTicker(time.Minute)
		if feedErr != nil {
			a.Log.Warn("initial scan incomplete; retrying", "error", feedErr)
			feedErr = nil
		}
		for fetchCtx.Err() == nil {
			select {
			case <-fetchCtx.Done():
			case <-ticker.C:
				// Periodic scans are additive. A deliberate bounded ingest owns pruning.
				_, feedErr = svc.Scan(fetchCtx, o.HighWater, o.LowWater)
				if feedErr == nil {
					feedErr = a.Jobs.ResumeSources(fetchCtx)
				}
			}
			if feedErr != nil {
				a.Log.Warn("source scan incomplete; retrying on next interval", "error", feedErr)
				feedErr = nil
			}
		}
		ticker.Stop()
	}
	var drainErr error
	if !o.Listen {
		drainErr = pipe.WaitDrained(workCtx, settle)
	}
	if o.Listen {
		pipe.Wait()
	}
	cancelErr := workCtx.Err()
	stopSignals()
	pipe.Wait()
	stopView()
	<-done
	fmt.Print(view.Summary())
	// Keep BM25 available during intake; build it if this is the first ingest.
	if err := a.DB.BuildSearchIndex(ctx); err != nil {
		return err
	}
	if feedErr != nil {
		return feedErr
	}
	if drainErr != nil {
		return drainErr
	}
	if cancelErr != nil {
		return cancelErr
	}
	return a.Jobs.CheckResolved(ctx)
}
