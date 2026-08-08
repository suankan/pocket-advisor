package doctor

import (
	"context"
	"fmt"
	"log/slog"
)

// ForgetTarget is a document and all its descendants, collected before
// any deletion begins. The collector walks the lineage tree from the
// root and gathers every Tier 1 URI and every doc_id so the deletions
// are deterministic and rerunnable.
type ForgetTarget struct {
	DocIDs      []string // root and all descendants
	RawURIs     []string // Tier 1 raw/ URIs to delete
	ExtractURIs []string // Tier 1 extracted/ URIs to delete
}

// LineageCollector walks a document tree to build a ForgetTarget. It
// reads only: the actual deletion belongs to the stores.
type LineageCollector interface {
	// Descendants returns all doc_ids that descend from rootDocID,
	// including the root itself, breadth-first.
	Descendants(ctx context.Context, rootDocID string) ([]string, error)
	// Tier1URIs returns all raw/ and extracted/ URIs for the given
	// doc_ids, partitioned into root and child.
	Tier1URIs(ctx context.Context, docIDs []string) (raw, extracted []string, err error)
}

// CollectForgetTarget gathers the full tree before deletion. This makes
// the operation idempotent: a rerun after partial failure finds the same
// set of targets and converges.
func CollectForgetTarget(ctx context.Context, collector LineageCollector, rootDocID string) (*ForgetTarget, error) {
	descendants, err := collector.Descendants(ctx, rootDocID)
	if err != nil {
		return nil, fmt.Errorf("collect descendants for %s: %w", rootDocID, err)
	}
	if len(descendants) == 0 {
		return nil, fmt.Errorf("document %s not found", rootDocID)
	}

	raw, extracted, err := collector.Tier1URIs(ctx, descendants)
	if err != nil {
		return nil, fmt.Errorf("collect tier1 uris: %w", err)
	}

	return &ForgetTarget{
		DocIDs:      descendants,
		RawURIs:     raw,
		ExtractURIs: extracted,
	}, nil
}

// ForgetDeleter performs the actual deletion across stores. It must be
// idempotent: every step tolerates absence (the object or row is already
// gone).
type ForgetDeleter interface {
	// DeleteDocIDs removes database rows for the given doc_ids. Cascades
	// handle chunks. Returns the number of rows deleted.
	DeleteDocIDs(ctx context.Context, docIDs []string) (int64, error)
	// DeleteObjects removes Tier 1 objects by key. Tolerates missing keys.
	DeleteObjects(ctx context.Context, keys []string) error
}

// ExecuteForget deletes every target. The order is database first,
// then objects: if the database deletion fails, the objects remain
// orphaned but queryable, which is safer than the reverse (objects
// gone, database still referencing them). Every step is idempotent.
func ExecuteForget(ctx context.Context, del ForgetDeleter, target *ForgetTarget, log *slog.Logger) error {
	if len(target.DocIDs) > 0 {
		n, err := del.DeleteDocIDs(ctx, target.DocIDs)
		if err != nil {
			return fmt.Errorf("delete database rows: %w", err)
		}
		if log != nil {
			log.Info("database rows deleted", "count", n)
		}
	}

	allKeys := append(append([]string{}, target.RawURIs...), target.ExtractURIs...)
	if len(allKeys) > 0 {
		if err := del.DeleteObjects(ctx, allKeys); err != nil {
			return fmt.Errorf("delete tier1 objects (re-run to converge): %w", err)
		}
		if log != nil {
			log.Info("tier1 objects deleted", "count", len(allKeys))
		}
	}

	return nil
}
