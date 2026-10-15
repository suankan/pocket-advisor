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
	"github.com/suankan/pocket-advisor/internal/discovery"
	"github.com/suankan/pocket-advisor/internal/pipeline"
	"github.com/suankan/pocket-advisor/internal/provision"
	"github.com/suankan/pocket-advisor/internal/source"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
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
	var incomplete *source.IncompleteError
	if errors.As(err, &incomplete) {
		for _, p := range incomplete.Deferred {
			fmt.Printf("deferred: %s\n", p)
		}
	}
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
	a, err := app.New(ctx, cfg, logs, app.Needs{Sources: true, Postgres: true, Metrics: true}, o.WorkspaceID)
	if err != nil {
		return err
	}
	defer a.Close()
	unlock, err := a.DB.LockMaintenance(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	// Failures recorded from here on are this run's; older ones are reported
	// but do not decide its outcome (Jobs.CheckResolved). The run summary is
	// scoped by the same instant.
	since, err := a.Jobs.Clock(ctx)
	if err != nil {
		return err
	}
	started := time.Now()
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
	pipe, err := pipeline.New(ctx, a, stats, embedder, pipeline.Options{OCRLangs: o.OCRLangs})
	if err != nil {
		return err
	}
	defer pipe.Close()
	// Work a dead process left RUNNING needs no startup step: its lease
	// expires and a pool claims it like any other job. Source waits are not
	// touched here: they are retried after the scan, when locations are current.
	// Workers run while the scan admits: a job commits together with the source
	// occurrence it reads, so new work is always resolvable. Only leftover work
	// for a renamed original can race a scan that has not reached its new path;
	// it parks as WAITING_SOURCE and feed retries it once the scan completes.
	// Ingestion never deletes; that is --reconcile-deletions' job, run on its own.
	workerFetch, stopWorkers := context.WithCancel(fetchCtx)
	defer stopWorkers()
	pipe.Start(workerFetch, workCtx)
	feedErr := feed(fetchCtx, svc, a.Jobs)
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
				feedErr = feed(fetchCtx, svc, a.Jobs)
			}
			if feedErr != nil {
				a.Log.Warn("source scan incomplete; retrying on next interval", "error", feedErr)
				feedErr = nil
			}
		}
		ticker.Stop()
	}
	var drainErr error
	if !o.Listen && fetchCtx.Err() == nil {
		// A failed scan still leaves valid admitted work; finish it and report
		// the scan error afterwards.
		drainErr = drain(workCtx, pipe, a.Jobs, feedErr == nil)
	}
	// A quiet queue is not a stop. End claiming, and join every handler before
	// building the search index.
	stopWorkers()
	pipe.Wait()
	runErr := errors.Join(feedErr, drainErr, fetchCtx.Err(), workCtx.Err())
	// Keep BM25 available during intake; create it once writes have landed.
	if fetchCtx.Err() == nil && workCtx.Err() == nil {
		runErr = errors.Join(runErr, a.DB.BuildSearchIndex(fetchCtx))
	}
	resolveErr := a.Jobs.CheckResolved(ctx, since)
	// Read from the store rather than counted in memory, so an interrupted or
	// failed run still says what it did. A summary that cannot be read does
	// not change the run's outcome.
	if summary, err := a.Jobs.RunSummary(ctx, since); err != nil {
		fmt.Fprintf(os.Stderr, "pocket-advisor: run summary unavailable: %v\n", err)
	} else {
		printRunSummary(os.Stdout, o.WorkspaceID, time.Since(started), summary)
	}
	return errors.Join(runErr, resolveErr)
}

// feed runs one admission pass: scan, resume jobless rows reachable from it,
// then retry source waits now that locations are current. A scan that
// deferred unstable originals still resumes from what it observed; a retried
// wait whose original is still missing parks again with its attempt refunded.
// The deferral is reported after the pass.
func feed(ctx context.Context, svc *discovery.Service, jobs *postgres.Jobs) error {
	scan, scanErr := svc.Scan(ctx)
	if scan == "" {
		return scanErr
	}
	if err := svc.ResumeLegacy(ctx, scan); err != nil {
		return errors.Join(scanErr, err)
	}
	return errors.Join(scanErr, jobs.ResumeSources(ctx))
}

// drain waits for the pipeline to go quiet. A worker can decide a source is
// unavailable just before the scan completes and record the wait just after
// feed retried it, so when the scan completed, waits are retried once more:
// originals that are genuinely gone park again immediately.
func drain(ctx context.Context, pipe *pipeline.Pipeline, jobs *postgres.Jobs, scanned bool) error {
	if err := pipe.WaitDrained(ctx, settle); err != nil || !scanned {
		return err
	}
	_, _, waiting, _, err := jobs.Counts(ctx)
	if err != nil || waiting == 0 {
		return err
	}
	if err := jobs.ResumeSources(ctx); err != nil {
		return err
	}
	return pipe.WaitDrained(ctx, settle)
}
