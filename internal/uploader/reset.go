package uploader

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/storage/rustfs"
)

// Resetter performs the destructive operations. It holds both stores because
// neither half is valid alone.
type Resetter struct {
	vault *rustfs.Vault
	docs  *postgres.DocumentRepo
	log   *slog.Logger
}

func NewResetter(v *rustfs.Vault, docs *postgres.DocumentRepo, log *slog.Logger) *Resetter {
	return &Resetter{vault: v, docs: docs, log: log}
}

// Wipe purges a workspace from Tier 1 and cascades into Tier 2.
//
// Tier 2 and Tier 3 are derivatives of Tier 1 objects. Purging the bucket
// while leaving the database populated leaves every rustfs_raw_uri dangling and
// every citation unresolvable — retrieval keeps returning confident results
// that point at nothing, which is worse than either a clean reset or no action
// at all.
//
// So the database is emptied FIRST. If Postgres is unreachable the bucket is
// never touched, and the failure leaves the system in a consistent state
// rather than half-reset (§5.1).
func (r *Resetter) Wipe(ctx context.Context, workspaceID string) error {
	if workspaceID == "" {
		return fmt.Errorf("refusing to wipe: no workspace specified")
	}

	rows, err := r.docs.DeleteWorkspace(ctx, workspaceID)
	if err != nil {
		return fmt.Errorf("wipe aborted, bucket untouched: %w", err)
	}
	r.log.Info("tier 2 cleared", "workspace_id", workspaceID, "documents_deleted", rows)

	prefix := domain.WorkspacePrefix(workspaceID)
	if _, err := r.vault.RemovePrefix(ctx, prefix); err != nil {
		return fmt.Errorf("tier 1 purge failed after tier 2 was cleared "+
			"(re-run --wipe to converge): %w", err)
	}
	r.log.Info("tier 1 purged", "workspace_id", workspaceID, "prefix", prefix)
	return nil
}

// Forget removes a single document by content hash, cascading the same way.
// This is the explicit alternative to inferring deletion from a file's absence
// in a later upload run.
func (r *Resetter) Forget(ctx context.Context, workspaceID, sha string) error {
	if len(sha) != 64 {
		return fmt.Errorf("--forget expects a 64-character sha256, got %d characters", len(sha))
	}

	rows, err := r.docs.DeleteBySHA(ctx, workspaceID, sha)
	if err != nil {
		return fmt.Errorf("forget aborted, bucket untouched: %w", err)
	}

	for _, key := range []string{
		domain.RawObjectKey(workspaceID, sha),
		domain.ExtractedObjectKey(workspaceID, sha),
	} {
		exists, _, err := r.vault.Exists(ctx, key)
		if err != nil {
			return err
		}
		if exists {
			if err := r.vault.Remove(ctx, key); err != nil {
				return err
			}
		}
	}

	r.log.Info("forgotten", "workspace_id", workspaceID, "sha256", sha, "documents_deleted", rows)
	return nil
}
