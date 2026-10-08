package postgres

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Listener holds one dedicated LISTEN connection, outside the application
// pool, on the workspace's own DSN and role. Notifications are hints: polling
// continues regardless, so a lost connection delays nothing for longer than a
// poll interval.
type Listener struct {
	cfg    *pgx.ConnConfig
	wake   func()
	logf   func(msg string, args ...any)
	wg     sync.WaitGroup
	cancel context.CancelFunc
}

// Reconnect backoff bounds. Internal constants, like every queue timing.
const (
	listenBackoffMin = time.Second
	listenBackoffMax = 30 * time.Second
)

// NewListener builds a listener whose connection settings are copied from the
// pool, so it always targets the same workspace database and role. wake is
// called on every notification and once after each reconnect, which covers
// notifications missed while disconnected.
func (d *DB) NewListener(wake func(), logf func(msg string, args ...any)) *Listener {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Listener{cfg: d.Pool.Config().ConnConfig.Copy(), wake: wake, logf: logf}
}

// Start establishes LISTEN before returning, so initial claims cannot miss a
// notification, then keeps the connection alive in the background. A failed
// first attempt is not fatal: the reconnect loop keeps trying while polling
// carries the work. Close joins the goroutine.
func (l *Listener) Start(ctx context.Context) {
	ctx, l.cancel = context.WithCancel(ctx)
	conn := l.connect(ctx)
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		l.run(ctx, conn)
	}()
}

func (l *Listener) connect(ctx context.Context) *pgx.Conn {
	cctx, cancel := context.WithTimeout(ctx, ControlTimeout)
	defer cancel()
	conn, err := pgx.ConnectConfig(cctx, l.cfg)
	if err != nil {
		l.logf("job listener cannot connect; polling continues", "error", err)
		return nil
	}
	if _, err := conn.Exec(cctx, "LISTEN "+NotifyChannel); err != nil {
		l.logf("job listener cannot LISTEN; polling continues", "error", err)
		_ = conn.Close(context.Background())
		return nil
	}
	return conn
}

func (l *Listener) run(ctx context.Context, conn *pgx.Conn) {
	backoff := listenBackoffMin
	for ctx.Err() == nil {
		if conn == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff/2 + time.Duration(rand.Int64N(int64(backoff/2)+1))):
			}
			conn = l.connect(ctx)
			if conn == nil {
				backoff = min(backoff*2, listenBackoffMax)
				continue
			}
			backoff = listenBackoffMin
			// Anything notified while disconnected is gone; look once.
			l.wake()
		}
		if _, err := conn.WaitForNotification(ctx); err != nil {
			_ = conn.Close(context.Background())
			conn = nil
			if ctx.Err() == nil {
				l.logf("job listener lost; reconnecting while polling continues", "error", err)
			}
			continue
		}
		l.wake()
	}
	if conn != nil {
		_ = conn.Close(context.Background())
	}
}

// Close stops the listener and joins its goroutine.
func (l *Listener) Close() {
	if l.cancel != nil {
		l.cancel()
	}
	l.wg.Wait()
}
