package cli

import (
	"context"
	"errors"
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
	return runIngestContext(context.Background(), o, cfg, logs)
}

func runIngestContext(ctx context.Context, o *Options, cfg *config.Config, logs *telemetry.Logs) error {
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
	stats := telemetry.NewStats()
	svc.Progress = &stats.Scan
	pipe, err := pipeline.New(ctx, a, stats, embedder, pipeline.Options{OCRLangs: o.OCRLangs})
	if err != nil {
		return err
	}
	defer pipe.Close()
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
	// Admission fills the durable outbox before any worker resolves a source.
	// This refreshes renamed paths without a preparatory binding traversal.
	scan, feedErr := svc.Scan(fetchCtx)
	if feedErr == nil {
		feedErr = svc.ResumeLegacy(fetchCtx, scan)
	}
	if feedErr == nil {
		feedErr = a.Jobs.Reconcile(fetchCtx, 0)
	}
	workerFetch, stopWorkers := context.WithCancel(fetchCtx)
	defer stopWorkers()
	if feedErr == nil || o.Listen {
		pipe.Start(workerFetch, workCtx)
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
				// Listener scans are additive and never authorize deletion.
				scan, feedErr = svc.Scan(fetchCtx)
				if feedErr == nil {
					feedErr = svc.ResumeLegacy(fetchCtx, scan)
				}
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
	if !o.Listen && feedErr == nil {
		drainErr = pipe.WaitDrained(workCtx, settle)
	}
	// A quiet queue is not a barrier. Stop dispatch/fetch and join every
	// handler before planning any deletion; retain the maintenance lock.
	stopWorkers()
	pipe.Wait()
	stopView()
	<-done
	runErr := errors.Join(feedErr, drainErr, fetchCtx.Err(), workCtx.Err())
	if o.IngestAll && !o.NoPrune {
		if runErr != nil {
			fmt.Println("prune skipped: source scan or ingestion was incomplete or interrupted")
		} else {
			if err := prune(fetchCtx, o, a, scan); err != nil {
				fmt.Printf("prune skipped: %v\n", err)
				runErr = errors.Join(runErr, err)
			}
		}
	}
	// Keep BM25 available during intake; create it after writes and deletion.
	if fetchCtx.Err() == nil && workCtx.Err() == nil {
		runErr = errors.Join(runErr, a.DB.BuildSearchIndex(fetchCtx))
	}
	fmt.Print(view.Summary())
	return errors.Join(runErr, a.Jobs.CheckResolved(ctx))
}
