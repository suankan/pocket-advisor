// Package app builds the dependency graph the whole process shares: config,
// per-role logging, metrics, store connections, and the CPU bound.
//
// It used to hold the wiring each worker binary repeated. There is one binary
// now, so it holds the wiring once — and the graph it builds is shared by every
// role rather than rebuilt per pod (ingestion-design.md §8.2).
package app

import (
	"context"
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

	// Two Tier 1 clients for two scoped identities. Vault is the worker
	// identity used by discovery and the extractors; Uploads is the uploader
	// identity, the only one permitted to write raw/ or delete (§5.1).
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
// ctx governs connection setup and is retained by the stores; the caller owns
// signal handling, because shutdown ordering is the supervisor's business.
func New(ctx context.Context, cfg *config.Config, logs *telemetry.Logs, needs Needs) (*App, error) {
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

	if needs.RustFS || needs.Uploader {
		if err := cfg.RequireRustFS(); err != nil {
			return nil, err
		}
	}

	if needs.RustFS {
		v, err := rustfs.NewWorker(cfg.RustFS)
		if err != nil {
			return nil, err
		}
		a.Vault = v
	}

	if needs.Uploader {
		u, err := rustfs.NewUploader(cfg.RustFS)
		if err != nil {
			return nil, err
		}
		// Only the uploader identity may create the bucket, so this is the one
		// place the check belongs.
		if err := u.EnsureBucket(ctx); err != nil {
			return nil, err
		}
		a.Uploads = u
	}

	if needs.Postgres {
		if err := cfg.RequirePostgres(); err != nil {
			return nil, err
		}
		db, err := postgres.Connect(ctx, cfg.Postgres.DSN, cfg.Postgres.MaxConns)
		if err != nil {
			return nil, err
		}
		a.DB = db
		a.Docs = postgres.NewDocumentRepo(db)
		a.Chunks = postgres.NewChunkRepo(db)
		a.stopFns = append(a.stopFns, db.Close)
	}

	if needs.NATS {
		b, err := bus.Connect(ctx, cfg.NATS.URL)
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
