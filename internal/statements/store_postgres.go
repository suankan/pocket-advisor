package statements

import (
	"context"
	"fmt"

	"github.com/suankan/pocket-advisor/internal/storage/postgres"
)

// PostgresStore reads documents rows for the closed bank-transactions
// collection set List resolves. It runs no ranking and no full-text search:
// every restriction is either the workspace/collection scope (registry
// membership) or a status/text-presence guard that also gates ordinary
// retrieval (ingestion-design.md §2.2).
type PostgresStore struct{ db *postgres.DB }

// NewPostgresStore wires the store to an already-open Tier 2 pool.
func NewPostgresStore(db *postgres.DB) *PostgresStore { return &PostgresStore{db: db} }

const candidatesSQL = `
SELECT doc_id::text, source_filename, collection_id, rustfs_raw_uri, raw_sha256,
       normalized_text
FROM documents
WHERE workspace_id = $1
  AND collection_id = ANY($2)
  AND processing_status = 'COMPLETED'
  AND normalized_text IS NOT NULL
  AND normalized_text <> ''
ORDER BY collection_id, source_filename`

func (p *PostgresStore) Candidates(ctx context.Context, workspaceID string, collectionIDs []string) ([]StoredDocument, error) {
	if len(collectionIDs) == 0 {
		return nil, nil
	}
	rows, err := p.db.Pool.Query(ctx, candidatesSQL, workspaceID, collectionIDs)
	if err != nil {
		return nil, fmt.Errorf("list statement documents: %w", err)
	}
	defer rows.Close()

	var out []StoredDocument
	for rows.Next() {
		var d StoredDocument
		if err := rows.Scan(&d.DocID, &d.Filename, &d.CollectionID, &d.RawURI, &d.SHA256, &d.Text); err != nil {
			return nil, fmt.Errorf("scan statement document: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read statement documents: %w", err)
	}
	return out, nil
}
