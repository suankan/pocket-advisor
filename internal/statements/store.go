package statements

import "context"

// candidate is one row read back from documents, before period filtering.
type candidate struct {
	docID, filename, collectionID, rawURI, sha256, text string
}

// Store reads candidate statement documents. The one production
// implementation is PostgresStore; tests substitute a fake so List's
// filtering, period, sorting, and truncation logic is exercised hermetically
// (internal/mailbox follows the same split for the same reason).
type Store interface {
	Candidates(ctx context.Context, workspaceID string, collectionIDs []string) ([]StoredDocument, error)
}

// StoredDocument is what the store owes List for one documents row: enough
// to build an evidence packet and to run period detection.
type StoredDocument struct {
	DocID, Filename, CollectionID, RawURI, SHA256, Text string
}
