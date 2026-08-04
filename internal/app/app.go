// Package app builds the dependency graph the whole process shares: config,
// per-role logging, metrics, store connections, and the CPU bound.
//
// It used to hold the wiring each worker binary repeated. There is one binary
// now, so it holds the wiring once — and the graph it builds is shared by every
// role rather than rebuilt per pod (ingestion-design.md §8.2).
package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/suankan/pocket-advisor/internal/bus"
	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/limits"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/storage/rustfs"
	"github.com/suankan/pocket-advisor/internal/telemetry"
)

type App struct {
	Cfg  *config.Config
	Logs *telemetry.Logs
	Log  *slog.Logger

	// Two Vaults over the same workspace-scoped RustFS identity and bucket
	// (workspace-isolation.md §2.2). Vault is constructed with RoleWorker and
	// Uploads with RoleUploader — the raw/-vs-extracted/ write-authority
	// split (§5.1) that used to be two separate RustFS identities is now
	// enforced by that Role field in application code (workspace-isolation.md §9).
	Vault   *rustfs.Vault
	Uploads *rustfs.Vault

	DB     *postgres.DB
	Docs   *postgres.DocumentRepo
	Chunks *postgres.ChunkRepo
	Bus    *bus.Bus

	// CPU is the single bound on genuinely CPU-bound work — rasterisation and
	// OCR — shared across every lane in the process.
	CPU *limits.CPU

	stopFns []func()
}

type Needs struct {
	RustFS   bool
	Uploader bool
	Postgres bool
	NATS     bool
	Metrics  bool
}

// New builds what the requested modes need and nothing more.
//
// workspaceID selects whose Postgres database, RustFS bucket, and NATS
// account to connect to (workspace-isolation.md §2) — every store this
// process touches is scoped to one workspace, never a shared identity.
// Required whenever any Need is set; New returns an error otherwise.
//
// ctx governs connection setup and is retained by the stores; the caller owns
// signal handling, because shutdown ordering is the supervisor's business.
func New(ctx context.Context, cfg *config.Config, logs *telemetry.Logs, needs Needs, workspaceID string) (*App, error) {
	a := &App{
		Cfg:  cfg,
		Logs: logs,
		Log:  logs.Logger(telemetry.RoleApp),
		CPU:  limits.NewCPU(limits.CPUs),
	}

	if needs.Metrics {
		shutdown := telemetry.ServeMetrics(cfg.MetricsPort, a.Log)
		a.stopFns = append(a.stopFns, func() { _ = shutdown(context.Background()) })
	}

	var w config.Workspace
	if workspaceID != "" {
		var err error
		w, err = cfg.Workspace(workspaceID)
		if err != nil {
			return nil, err
		}
	}

	if (needs.RustFS || needs.Uploader || needs.Postgres || needs.NATS) && workspaceID == "" {
		return nil, fmt.Errorf("a workspace id is required")
	}

	if needs.RustFS {
		v, err := rustfs.NewForWorkspace(cfg.RustFS, w.BucketName, w.RustFSAccessKey, w.RustFSSecretKey, rustfs.RoleWorker)
		if err != nil {
			return nil, err
		}
		a.Vault = v
	}

	if needs.Uploader {
		u, err := rustfs.NewForWorkspace(cfg.RustFS, w.BucketName, w.RustFSAccessKey, w.RustFSSecretKey, rustfs.RoleUploader)
		if err != nil {
			return nil, err
		}
		// The bucket already exists from --create-workspace; this is a
		// harmless, idempotent safety net, not the primary creation path.
		if err := u.EnsureBucket(ctx); err != nil {
			return nil, err
		}
		a.Uploads = u
	}

	if needs.Postgres {
		dsn, err := cfg.WorkspacePostgresDSN(workspaceID)
		if err != nil {
			return nil, err
		}
		db, err := postgres.Connect(ctx, dsn, cfg.Postgres.MaxConns)
		if err != nil {
			return nil, err
		}
		a.DB = db
		a.Docs = postgres.NewDocumentRepo(db)
		a.Chunks = postgres.NewChunkRepo(db)
		a.stopFns = append(a.stopFns, db.Close)
	}

	if needs.NATS {
		b, err := bus.Connect(ctx, cfg.NATS.URL, w.NATSUser, w.NATSPassword)
		if err != nil {
			return nil, err
		}
		if err := b.EnsureStreams(ctx); err != nil {
			return nil, err
		}
		a.Bus = b
		a.stopFns = append(a.stopFns, b.Close)
	}

	a.Log.Info("dependencies ready",
		"cpus", limits.CPUs,
		"workspace_id", workspaceID,
		"rustfs", cfg.RustFS.Endpoint,
		"nats", cfg.NATS.URL,
		"metrics_port", cfg.MetricsPort)
	return a, nil
}

// Logger returns the log file writer for one role.
func (a *App) Logger(role string) *slog.Logger { return a.Logs.Logger(role) }

func (a *App) Close() {
	for i := len(a.stopFns) - 1; i >= 0; i-- {
		a.stopFns[i]()
	}
}
