package retention

import (
	"context"
	"encoding/hex"
	"fmt"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"log/slog"
)

type QueuePurger interface{ PurgeQueues(context.Context) error }
type Resetter struct {
	docs   *postgres.DocumentRepo
	queues QueuePurger
	log    *slog.Logger
}

func NewResetter(docs *postgres.DocumentRepo, queues QueuePurger, log *slog.Logger) *Resetter {
	return &Resetter{docs, queues, log}
}
func (r *Resetter) Wipe(ctx context.Context, workspaceID string) error {
	if workspaceID == "" {
		return fmt.Errorf("workspace is required")
	}
	n, err := r.docs.DeleteWorkspace(ctx)
	if err != nil {
		return err
	}
	r.log.Info("derived documents removed", "count", n)
	if r.queues != nil {
		return r.queues.PurgeQueues(ctx)
	}
	return nil
}
func (r *Resetter) Forget(ctx context.Context, workspaceID, sha string) error {
	if workspaceID == "" {
		return fmt.Errorf("workspace is required")
	}
	b, err := hex.DecodeString(sha)
	if err != nil || len(b) != 32 {
		return fmt.Errorf("forget requires a SHA-256 digest")
	}
	_, err = r.docs.DeleteBySHA(ctx, sha)
	return err
}
