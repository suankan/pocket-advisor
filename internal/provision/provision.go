// Package provision creates and destroys the physical per-workspace
// isolation boundary — a Postgres database and role, a RustFS bucket and
// identity, and a NATS account and user, all named by workspace id
// (docs/workspace-isolation.md). It is deliberately plain, transport-agnostic
// Go: internal/cli calls it today, and nothing about that has to change if
// an API server calls it instead later (docs/api-server-design.md §2).
package provision

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/suankan/pocket-advisor/internal/config"
)

// CreateWorkspace provisions all three stores for id, in order of cheapest
// and most authoritative first (workspace-isolation.md §6): Postgres, then
// RustFS, then NATS last, since NATS is both the riskiest step (it touches
// the Kubernetes API and restarts a shared pod) and the slowest.
//
// On failure at any step, everything this call created is rolled back —
// not anything that already existed before it, since every delete step
// tolerates "already absent" and every create step tolerates "already
// present" (workspace-isolation.md §2, "idempotent either way"). A retry
// after a rollback is indistinguishable from a first attempt.
func CreateWorkspace(ctx context.Context, cfg *config.Config, id string, log *slog.Logger) error {
	if id == "" {
		return fmt.Errorf("workspace id is required")
	}
	if _, err := cfg.Workspace(id); err != nil {
		return err
	}

	if err := createPostgres(ctx, cfg, id, log); err != nil {
		return fmt.Errorf("postgres: %w", err)
	}

	if err := createRustFS(ctx, cfg, id, log); err != nil {
		rollbackErr := deletePostgres(ctx, cfg, id, log)
		return joinRollback(fmt.Errorf("rustfs: %w", err), rollbackErr)
	}

	if err := createNATS(ctx, cfg, id, log); err != nil {
		rollbackErr := deleteRustFS(ctx, cfg, id, log)
		if rollbackErr == nil {
			rollbackErr = deletePostgres(ctx, cfg, id, log)
		} else if err2 := deletePostgres(ctx, cfg, id, log); err2 != nil {
			rollbackErr = fmt.Errorf("%w; also: %w", rollbackErr, err2)
		}
		return joinRollback(fmt.Errorf("nats: %w", err), rollbackErr)
	}

	// Last, because it is the only step that restarts a store: the three
	// stores must exist before RustFS is told where to publish their events.
	// Not rolled back on failure — a workspace whose stores exist but whose
	// notifications are not wired is still usable via the scan, so tearing it
	// all down would be a worse outcome than a loud error.
	if err := configureNotify(ctx, cfg, id, log); err != nil {
		return fmt.Errorf("rustfs notify: %w", err)
	}

	log.Info("workspace created", "workspace_id", id)
	return nil
}

// DeleteWorkspace tears down all three stores for id, in the reverse order
// of creation (workspace-isolation.md §7): NATS first, so no new work can be
// enqueued against the workspace while the rest of the teardown runs;
// Postgres next, since it is the authoritative answer to "does this
// workspace's data still exist" (the same reasoning
// internal/uploader/reset.go's Wipe already uses); RustFS last.
//
// Each step is attempted in order. A failure stops the sequence rather than
// continuing past it into an irreversible next step — the same posture as
// Wipe refusing to touch the bucket if Postgres is unreachable.
func DeleteWorkspace(ctx context.Context, cfg *config.Config, id string, log *slog.Logger) error {
	if id == "" {
		return fmt.Errorf("workspace id is required")
	}

	// Before NATS, because the notify secret holds that account's password:
	// leaving it behind after the account is gone points RustFS at
	// credentials that no longer authenticate.
	if err := deleteNotifySecret(ctx, cfg, id, log); err != nil {
		return fmt.Errorf("rustfs notify: %w (nothing else touched)", err)
	}
	if err := deleteNATS(ctx, cfg, id, log); err != nil {
		return fmt.Errorf("nats: %w (postgres and rustfs left untouched)", err)
	}
	if err := deletePostgres(ctx, cfg, id, log); err != nil {
		return fmt.Errorf("postgres: %w (rustfs left untouched)", err)
	}
	if err := deleteRustFS(ctx, cfg, id, log); err != nil {
		return fmt.Errorf("rustfs: %w", err)
	}

	log.Info("workspace deleted", "workspace_id", id)
	return nil
}

// joinRollback reports the original failure, plus whether the rollback
// itself also failed — which the operator needs to know, because it means
// manual cleanup may be required rather than a clean retry.
func joinRollback(cause, rollbackErr error) error {
	if rollbackErr == nil {
		return cause
	}
	return fmt.Errorf("%w; rollback also failed, manual cleanup may be needed: %w", cause, rollbackErr)
}
