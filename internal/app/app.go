// Package app holds the wiring every worker binary repeats: config, logging,
// metrics, store connections, and graceful shutdown. cmd/ mains stay as thin
// as §8.2 requires.
package app

import (
	"context"
	"log/slog"
	"os/signal"
	"syscall"

	"github.com/suankan/pocket-advisor/internal/bus"
	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/storage/minio"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/telemetry"
)

type App struct {
	Cfg      *config.Config
	Log      *slog.Logger
	Vault    *minio.Vault
	DB       *postgres.DB
	Docs     *postgres.DocumentRepo
	Chunks   *postgres.ChunkRepo
	Bus      *bus.Bus
	Ctx      context.Context
	stopFns  []func()
	stopSigs context.CancelFunc
}

type Needs struct {
	MinIO    bool
	Postgres bool
	NATS     bool
}

// New builds the dependency graph a worker needs and nothing more.
func New(name string, needs Needs) (*App, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	log := telemetry.NewLogger(name, cfg.LogLevel)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	a := &App{Cfg: cfg, Log: log, Ctx: ctx, stopSigs: cancel}

	shutdownMetrics := telemetry.ServeMetrics(cfg.MetricsPort, log)
	a.stopFns = append(a.stopFns, func() { _ = shutdownMetrics(context.Background()) })

	if needs.MinIO {
		if err := cfg.RequireMinIO(); err != nil {
			return nil, err
		}
		v, err := minio.New(cfg.MinIO)
		if err != nil {
			return nil, err
		}
		if err := v.EnsureBucket(ctx); err != nil {
			return nil, err
		}
		a.Vault = v
	}

	if needs.Postgres {
		if err := cfg.RequirePostgres(); err != nil {
			return nil, err
		}
		db, err := postgres.Connect(ctx, cfg.Postgres.DSN)
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

	log.Info("started", "metrics_port", cfg.MetricsPort)
	return a, nil
}

func (a *App) Close() {
	a.stopSigs()
	for i := len(a.stopFns) - 1; i >= 0; i-- {
		a.stopFns[i]()
	}
}
