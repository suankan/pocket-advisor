package postgres

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type executor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Begin(context.Context) (pgx.Tx, error)
}
type transactionKey struct{ db *DB }

// IsDeadlock reports whether PostgreSQL aborted a transaction to break a lock
// cycle (deadlock_detected). That says nothing about the work itself: the
// transaction it lost to has finished or is about to, and the same work rerun
// is expected to succeed. Every transaction runs at READ COMMITTED, so a
// serialization failure cannot occur and is not treated as one.
func IsDeadlock(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "40P01"
}

// IsDataException reports whether PostgreSQL refused a statement because of
// the values it carried (SQLSTATE class 22), such as an invalid byte sequence
// in text. The store is healthy; that input cannot be stored as given.
func IsDataException(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && strings.HasPrefix(pg.Code, "22")
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
	c, err := d.Pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	var ok bool
	err = c.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtext('source-ingestion-maintenance'))").Scan(&ok)
	if err != nil || !ok {
		c.Release()
		if err == nil {
			err = ErrWorkspaceBusy
		}
		return nil, err
	}
	return func() {
		_, _ = c.Exec(context.Background(), "SELECT pg_advisory_unlock(hashtext('source-ingestion-maintenance'))")
		c.Release()
	}, nil
}
