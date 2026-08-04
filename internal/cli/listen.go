package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/suankan/pocket-advisor/internal/app"
	"github.com/suankan/pocket-advisor/internal/client/embedding"
	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/dashboard"
	"github.com/suankan/pocket-advisor/internal/pipeline"
	"github.com/suankan/pocket-advisor/internal/telemetry"
)

// runListen is the live-event test mode (§5.2): it starts the same pipeline
// --ingest-all does, including the RustFS-events role, but never uploads,
// never scans, and never exits on idle — only an interrupt ends it. Proving
// the live path works, or not, is the entire point of this mode; --scan
// remains the reconciliation backstop, run separately.
func runListen(o *Options, cfg *config.Config, logs *telemetry.Logs) error {
	ctx := context.Background()

	a, err := app.New(ctx, cfg, logs, app.Needs{RustFS: true, Postgres: true, NATS: true, Metrics: true}, o.WorkspaceID)
	if err != nil {
		return err
	}
	defer a.Close()

	// Matters more here than anywhere else: this mode consumes nothing but
	// notify events, so a target aimed elsewhere makes it sit at zero forever
	// while quietly filling another workspace's account (§5.2).
	if err := checkNotifyTarget(ctx, cfg, o.WorkspaceID, a.Log); err != nil {
		return err
	}

	if err := cfg.RequireEmbedding(); err != nil {
		return err
	}
	embedder := embedding.New(cfg.Embedding)

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

	pipe.Start(fetchCtx, workCtx)
	a.Log.Info("listening for RustFS live notify events", "workspace_id", o.WorkspaceID)

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

	// Unlike --ingest-all/--scan/--reconcile, nothing here ever finishes on its
	// own — there is no upload or scan to drain, and idleness is the expected
	// steady state between events, not a completion signal. Only an interrupt
	// (fetchCtx cancellation, the first stage of the two-stage stop in
	// interrupts()) ends the wait; pipe.Wait() then blocks for the same
	// graceful drain / hard-abort sequence every other mode already uses.
	<-fetchCtx.Done()
	pipe.Wait()

	stopSignals()
	stopView()
	<-viewDone

	fmt.Print(view.Summary())
	fmt.Printf("logs      %s\n", logs.Dir())
	return nil
}
