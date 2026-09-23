package postgres

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"sync"
	"sync/atomic"
)

type executor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Begin(context.Context) (pgx.Tx, error)
}
type transactionKey struct{ db *DB }

type maintenanceLease struct {
	scanGeneration atomic.Uint64
}

func (d *DB) executor(ctx context.Context) executor {
	if tx, ok := ctx.Value(transactionKey{d}).(pgx.Tx); ok {
		return tx
	}
	return d.Pool
}
func (d *DB) Transaction(ctx context.Context, fn func(context.Context) error) error {
	tx, err := d.executor(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err = fn(context.WithValue(ctx, transactionKey{d}, tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// LockMaintenance excludes concurrent ingestion and destructive maintenance in
// this workspace database. The dedicated connection is released on shutdown.
func (d *DB) LockMaintenance(ctx context.Context) (func(), error) {
	lease := &maintenanceLease{}
	if !d.maintenance.CompareAndSwap(nil, lease) {
		return nil, ErrWorkspaceBusy
	}
	c, err := d.Pool.Acquire(ctx)
	if err != nil {
		d.maintenance.CompareAndSwap(lease, nil)
		return nil, err
	}
	var ok bool
	err = c.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtext('source-ingestion-maintenance'))").Scan(&ok)
	if err != nil || !ok {
		c.Release()
		d.maintenance.CompareAndSwap(lease, nil)
		if err == nil {
			err = ErrWorkspaceBusy
		}
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_, _ = c.Exec(context.Background(), "SELECT pg_advisory_unlock(hashtext('source-ingestion-maintenance'))")
			c.Release()
			d.maintenance.CompareAndSwap(lease, nil)
		})
	}, nil
}
