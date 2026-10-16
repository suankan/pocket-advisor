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

	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/limits"
	"github.com/suankan/pocket-advisor/internal/source"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/telemetry"
	"github.com/suankan/pocket-advisor/internal/workspace"
)

type App struct {
	Cfg  *config.Config
	Logs *telemetry.Logs
	Log  *slog.Logger

	Sources *source.Resolver
	// IgnoreMember reports whether a container member, by its own name, is
	// excluded by the workspace's ignore rules.
	IgnoreMember func(name string) bool
	Inventory    *postgres.SourceRepo
	Jobs         *postgres.Jobs

	DB     *postgres.DB
	Docs   *postgres.DocumentRepo
	Chunks *postgres.ChunkRepo
	Emails *postgres.EmailRepo

	// CPU is the single bound on genuinely CPU-bound work — rasterisation and
	// OCR — shared across every lane in the process.
	CPU *limits.CPU

	stopFns []func()
}

type Needs struct {
	Sources  bool
	Postgres bool
	Metrics  bool
}

// New builds what the requested modes need and nothing more.
//
// workspaceID selects whose Postgres database and source roots to use
// (workspace-isolation.md §2) — every store this process touches is scoped to
// one workspace, never a shared identity.
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

	if workspaceID != "" {
		if _, err := cfg.Workspace(workspaceID); err != nil {
			return nil, err
		}
	}

	if (needs.Sources || needs.Postgres) && workspaceID == "" {
		return nil, fmt.Errorf("a workspace id is required")
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
		if err := db.AssertWorkspaceScope(ctx, workspaceID); err != nil {
			db.Close()
			return nil, err
		}
		a.DB = db
		a.Docs = postgres.NewDocumentRepo(db)
		a.Chunks = postgres.NewChunkRepo(db)
		a.Emails = postgres.NewEmailRepo(db)
		a.Inventory = postgres.NewSourceRepo(db)
		a.Jobs = postgres.NewJobs(db)
		a.stopFns = append(a.stopFns, db.Close)
	}

	if needs.Sources {
		ws, err := workspace.Load(cfg.WorkspacesConfigPath, workspaceID)
		if err != nil {
			a.Close()
			return nil, err
		}
		a.Sources, err = source.New(ws.AbsPaths, ws.Ignore.Match, a.Inventory)
		if err != nil {
			a.Close()
			return nil, err
		}
		a.IgnoreMember = ws.Ignore.MatchName
		a.stopFns = append(a.stopFns, a.Sources.Close)
	}

	a.Log.Info("dependencies ready",
		"cpus", limits.CPUs,
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
