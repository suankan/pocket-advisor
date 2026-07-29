// Package postgres holds Tier 2 (relational lineage) and Tier 3 (vector
// index). All SQL lives behind the repositories in this package.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type DB struct {
	Pool *pgxpool.Pool
}

// Connect opens the Tier 2/3 pool.
//
// maxConns has to cover every lane that can hold a connection at once. pgxpool
// defaults to max(4, NumCPU), which was adequate when each role was its own pod
// with its own pool, but becomes the pipeline's narrowest point once all roles
// share one process — lanes would queue on connection acquisition rather than
// on the work itself.
func Connect(ctx context.Context, dsn string, maxConns int32) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	cfg.MaxConnLifetime = time.Hour
	cfg.HealthCheckPeriod = 30 * time.Second
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	// A warm floor, so the first burst of lanes does not all pay connection
	// setup at once.
	cfg.MinConns = 4

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &DB{Pool: pool}, nil
}

func (d *DB) Close() { d.Pool.Close() }

// PoolStats reports acquired and total connections for the dashboard.
func (d *DB) PoolStats() (acquired, max int32) {
	s := d.Pool.Stat()
	return s.AcquiredConns(), s.MaxConns()
}
