package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/suankan/pocket-advisor/internal/domain"
)

// EmailRepo owns the durable email browse metadata: the structured message
// row, its parsed mailboxes and reply headers, and the workspace's identifier
// graph (ingestion-design.md §2.5).
type EmailRepo struct{ db *DB }

func NewEmailRepo(db *DB) *EmailRepo { return &EmailRepo{db: db} }

// SaveEmailMessage persists one message's browse metadata and reports the
// conversation it was assigned to.
//
// One transaction, because the pieces are not independently useful: a message
// row naming a conversation whose identifier nodes were not written would place
// the message in a conversation nothing else can be found through, and half a
// merge would split one conversation into two.
//
// Idempotent for re-ingestion of the same doc_id: the message row is upserted,
// mailboxes and reply headers are replaced rather than appended (at-least-once
// delivery guarantees a document is eventually processed twice), and identifier
// nodes converge on the same component because the component is derived from
// the identifiers rather than minted per run.
// SaveEmailMessage records what the message says about itself. There is no
// grouping decision and therefore no workspace-wide lock: the ON CONFLICT
// upsert takes this document's row before its address and reference rows are
// replaced, which serialises concurrent writes to the same document and leaves
// unrelated messages free to proceed in parallel.
//
// Equal Message-IDs do not merge documents or elect an owner. Both rows are
// kept, and whether an identifier is currently shared is a question the
// indexed rows answer when read, not a fact frozen into whichever copy was
// written first.
func (r *EmailRepo) SaveEmailMessage(ctx context.Context, m domain.EmailMessage) error {
	tx, err := r.db.executor(ctx).Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin email metadata: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	encoded, err := json.Marshal(orEmptyWarnings(m.Warnings))
	if err != nil {
		return fmt.Errorf("marshal parse warnings %s: %w", m.DocID, err)
	}

	var sentAt any
	if !m.SentAt.IsZero() {
		sentAt = m.SentAt
	}

	// ingested_at is deliberately not touched on conflict. It is the snapshot
	// watermark a stable cursor pages against, and reprocessing a document
	// does not make it newly received — moving it would make a message the
	// caller has already seen reappear at the head of the next page.
	if _, err := tx.Exec(ctx, `
        INSERT INTO email_messages (
            doc_id, message_id, subject_raw,
            sent_at, automated_class, list_id, parse_warnings, parse_version)
        VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8)
        ON CONFLICT (doc_id) DO UPDATE SET
            message_id      = EXCLUDED.message_id,
            subject_raw     = EXCLUDED.subject_raw,
            sent_at         = EXCLUDED.sent_at,
            automated_class = EXCLUDED.automated_class,
            list_id         = EXCLUDED.list_id,
            parse_warnings  = EXCLUDED.parse_warnings,
            parse_version   = EXCLUDED.parse_version`,
		m.DocID, m.MessageID, m.SubjectRaw,
		sentAt, string(m.AutomatedClass), m.ListID,
		string(encoded), domain.EmailParseVersion,
	); err != nil {
		return fmt.Errorf("save email message %s: %w", m.DocID, err)
	}

	if err := replaceAddresses(ctx, tx, m); err != nil {
		return err
	}
	if err := replaceReferences(ctx, tx, m); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit email metadata %s: %w", m.DocID, err)
	}
	return nil
}

func replaceAddresses(ctx context.Context, tx pgx.Tx, m domain.EmailMessage) error {
	if _, err := tx.Exec(ctx, `DELETE FROM email_addresses WHERE doc_id = $1`, m.DocID); err != nil {
		return fmt.Errorf("clear addresses %s: %w", m.DocID, err)
	}
	if len(m.Addresses) == 0 {
		return nil
	}

	kinds := make([]string, len(m.Addresses))
	ordinals := make([]int32, len(m.Addresses))
	addresses := make([]string, len(m.Addresses))
	names := make([]string, len(m.Addresses))
	raws := make([]string, len(m.Addresses))
	valid := make([]bool, len(m.Addresses))
	for i, a := range m.Addresses {
		kinds[i] = string(a.Kind)
		ordinals[i] = int32(a.Ordinal)
		addresses[i] = a.Address
		names[i] = a.DisplayName
		raws[i] = a.Raw
		valid[i] = a.Valid
	}

	if _, err := tx.Exec(ctx, `
        INSERT INTO email_addresses (doc_id, kind, ordinal, address, display_name, raw, valid)
        SELECT $1, v.kind, v.ordinal, v.address, v.display_name, v.raw, v.valid
        FROM unnest($2::text[], $3::int[], $4::text[], $5::text[], $6::text[], $7::bool[])
             AS v(kind, ordinal, address, display_name, raw, valid)`,
		m.DocID, kinds, ordinals, addresses, names, raws, valid); err != nil {
		return fmt.Errorf("insert addresses %s: %w", m.DocID, err)
	}
	return nil
}

// replaceReferences rewrites a document's reply headers as written, preserving
// header order through ordinal.
func replaceReferences(ctx context.Context, tx pgx.Tx, m domain.EmailMessage) error {
	if _, err := tx.Exec(ctx, `DELETE FROM email_references WHERE doc_id = $1`, m.DocID); err != nil {
		return fmt.Errorf("clear references %s: %w", m.DocID, err)
	}
	if len(m.References) == 0 {
		return nil
	}

	kinds := make([]string, len(m.References))
	ordinals := make([]int32, len(m.References))
	messageIDs := make([]string, len(m.References))
	for i, ref := range m.References {
		kinds[i] = string(ref.Kind)
		ordinals[i] = int32(ref.Ordinal)
		messageIDs[i] = ref.MessageID
	}

	if _, err := tx.Exec(ctx, `
        INSERT INTO email_references (doc_id, kind, ordinal, message_id)
        SELECT $1, v.kind, v.ordinal, v.message_id
        FROM unnest($2::text[], $3::int[], $4::text[]) AS v(kind, ordinal, message_id)`,
		m.DocID, kinds, ordinals, messageIDs); err != nil {
		return fmt.Errorf("insert references %s: %w", m.DocID, err)
	}
	return nil
}

// EmailDocumentQuery selects the workspace's email message documents for a
// metadata reprocessing pass (ingestion-design.md §2.5).
//
// After is the exclusive lower bound of a keyset walk, empty on the first
// batch. Ordering is by doc_id, which is stable once assigned, so a run
// interrupted at any point resumes over the same sequence rather than a
// re-shuffled one.
type EmailDocumentQuery struct {
	After string
	Limit int
	// OnlyMissing restricts the walk to documents that have no email_messages
	// row yet — the cheap pass after an interrupted rebuild. It is a selection
	// narrowing, never a correctness one: a full pass rewrites every message
	// from its original bytes and converges on the same rows.
	OnlyMissing bool
}

// EmailDocuments lists the workspace's email message documents in
// deterministic doc_id order.
//
// Only doc_type = 'email' is returned. An archive is a container routed to the
// same worker but is not a message and has no metadata of its own, and a
// document with no original source is still returned so the reprocessor can
// report it as unreadable rather than silently losing the missing metadata.
func (r *EmailRepo) EmailDocuments(ctx context.Context, q EmailDocumentQuery) ([]domain.Document, error) {
	rows, err := r.db.executor(ctx).Query(ctx, `
        SELECT d.doc_id::text, d.mime_type, d.source_ref, d.raw_sha256
        FROM documents d
        WHERE d.doc_type = 'email'
          AND ($1::text = '' OR d.doc_id > $1::uuid)
          AND (NOT $2::bool OR NOT EXISTS (
                SELECT 1 FROM email_messages m WHERE m.doc_id = d.doc_id))
        ORDER BY d.doc_id
        LIMIT $3`, q.After, q.OnlyMissing, q.Limit)
	if err != nil {
		return nil, fmt.Errorf("list email documents: %w", err)
	}
	defer rows.Close()

	var out []domain.Document
	for rows.Next() {
		var d domain.Document
		if err := rows.Scan(&d.DocID, &d.MimeType, &d.SourceRef, &d.RawSHA256); err != nil {
			return nil, fmt.Errorf("list email documents: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func orEmptyWarnings(w []domain.EmailParseWarning) []domain.EmailParseWarning {
	if w == nil {
		return []domain.EmailParseWarning{}
	}
	return w
}
