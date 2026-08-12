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

// emailGraphLockKey is a fixed advisory lock key naming what is serialised —
// the one identifier graph this database holds — rather than which
// workspace, since each workspace is its own database (deviation 34) and an
// advisory lock is already scoped to the connection's own database.
const emailGraphLockKey = "email-identifier-graph"

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
func (r *EmailRepo) SaveEmailMessage(ctx context.Context, m domain.EmailMessage) (domain.EmailConversation, error) {
	var conv domain.EmailConversation

	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return conv, fmt.Errorf("begin email metadata: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Component maintenance reads the graph, decides a merge, and rewrites
	// rows the decision was based on. Two workers folding messages of one
	// conversation in concurrently would each plan against a graph the other
	// is changing, so the whole decision is serialised. It is a
	// transaction-scoped lock, released by commit or rollback.
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1))`, emailGraphLockKey); err != nil {
		return conv, fmt.Errorf("lock email graph: %w", err)
	}

	conv, err = assignConversation(ctx, tx, m)
	if err != nil {
		return conv, err
	}

	warnings := m.Warnings
	if conv.DuplicateOf != "" {
		// Recorded against this document, not the one that owns the
		// identifier: the defect is that this copy exists, and the owner's row
		// is not rewritten by someone else's ingest.
		warnings = append(append([]domain.EmailParseWarning{}, warnings...),
			domain.EmailParseWarning{
				Code:   domain.WarnDuplicateMessageID,
				Header: "Message-ID",
				Value:  m.MessageID,
			})
	}
	encoded, err := json.Marshal(orEmptyWarnings(warnings))
	if err != nil {
		return conv, fmt.Errorf("marshal parse warnings %s: %w", m.DocID, err)
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
            doc_id, message_id, subject_raw, subject_normalized,
            sent_at, automated_class, list_id, conversation_id,
            conversation_method, parse_warnings, parse_version)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11)
        ON CONFLICT (doc_id) DO UPDATE SET
            message_id          = EXCLUDED.message_id,
            subject_raw         = EXCLUDED.subject_raw,
            subject_normalized  = EXCLUDED.subject_normalized,
            sent_at             = EXCLUDED.sent_at,
            automated_class     = EXCLUDED.automated_class,
            list_id             = EXCLUDED.list_id,
            conversation_id     = EXCLUDED.conversation_id,
            conversation_method = EXCLUDED.conversation_method,
            parse_warnings      = EXCLUDED.parse_warnings,
            parse_version       = EXCLUDED.parse_version`,
		m.DocID, m.MessageID, m.SubjectRaw, m.SubjectNormalized,
		sentAt, string(m.AutomatedClass), m.ListID, conv.ConversationID,
		string(conv.Method), string(encoded), domain.EmailParseVersion,
	); err != nil {
		return conv, fmt.Errorf("save email message %s: %w", m.DocID, err)
	}

	if err := replaceAddresses(ctx, tx, m); err != nil {
		return conv, err
	}
	if err := replaceReferences(ctx, tx, m); err != nil {
		return conv, err
	}

	if err := tx.Commit(ctx); err != nil {
		return conv, fmt.Errorf("commit email metadata %s: %w", m.DocID, err)
	}
	return conv, nil
}

// assignConversation folds the message into the workspace's identifier graph,
// or applies the labelled fallback when it carries no identifier at all.
func assignConversation(ctx context.Context, tx pgx.Tx, m domain.EmailMessage) (domain.EmailConversation, error) {
	ids := m.Identifiers()
	if len(ids) == 0 {
		// A header orphan. Subject grouping is a guess, so it is stored under
		// its own method and never merged into an identifier component. It also
		// needs a participant to be worth making: a normalized subject on its
		// own recurs across unrelated correspondents, so without a usable
		// sender the message is a conversation of one rather than a member of a
		// bucket keyed on a common phrase.
		if sender := m.PrimarySender(); m.SubjectNormalized != "" && sender != "" {
			return domain.EmailConversation{
				ConversationID: domain.NewEmailSubjectConversationID(
					m.SubjectNormalized, sender),
				Method: domain.ConversationBySubject,
			}, nil
		}
		return domain.EmailConversation{
			ConversationID: domain.NewEmailIsolatedConversationID(m.DocID),
			Method:         domain.ConversationIsolated,
		}, nil
	}

	existing, err := loadIdentifierNodes(ctx, tx, ids)
	if err != nil {
		return domain.EmailConversation{}, err
	}
	seed := domain.NewEmailComponentID(smallestIdentifier(ids))
	plan := planComponent(m.DocID, m.MessageID, ids, existing, seed)

	if len(plan.MergeFrom) > 0 {
		// By component, not by identifier: merging two conversations has to
		// carry every node and every message already in them, including the
		// ancestors neither of these two messages mentioned.
		if _, err := tx.Exec(ctx, `
            UPDATE email_identifier_nodes SET component_id = $1
            WHERE component_id = ANY($2::uuid[])`,
			plan.ComponentID, plan.MergeFrom); err != nil {
			return domain.EmailConversation{}, fmt.Errorf("merge identifier components: %w", err)
		}
		if _, err := tx.Exec(ctx, `
            UPDATE email_messages SET conversation_id = $1
            WHERE conversation_id = ANY($2::uuid[])`,
			plan.ComponentID, plan.MergeFrom); err != nil {
			return domain.EmailConversation{}, fmt.Errorf("merge conversations: %w", err)
		}
	}

	if len(plan.Insert) > 0 {
		msgIDs := make([]string, len(plan.Insert))
		docIDs := make([]string, len(plan.Insert))
		for i, n := range plan.Insert {
			msgIDs[i], docIDs[i] = n.MessageID, n.DocID
		}
		// An empty doc_id is a placeholder ancestor: the identifier is known,
		// the message behind it is not. NULLIF keeps that as a NULL reference
		// rather than an invented document.
		if _, err := tx.Exec(ctx, `
            INSERT INTO email_identifier_nodes (message_id, doc_id, component_id)
            SELECT v.message_id, NULLIF(v.doc_id, '')::uuid, $1::uuid
            FROM unnest($2::text[], $3::text[]) AS v(message_id, doc_id)
            ON CONFLICT (message_id) DO NOTHING`,
			plan.ComponentID, msgIDs, docIDs); err != nil {
			return domain.EmailConversation{}, fmt.Errorf("insert identifier nodes: %w", err)
		}
	}

	if plan.AdoptOwn {
		// The message an earlier reply only named has now been ingested. The
		// doc_id IS NULL guard keeps this to placeholders: a node already
		// backed by a document is never retargeted.
		if _, err := tx.Exec(ctx, `
            UPDATE email_identifier_nodes SET doc_id = $1
            WHERE message_id = $2 AND doc_id IS NULL`,
			m.DocID, m.MessageID); err != nil {
			return domain.EmailConversation{}, fmt.Errorf("adopt placeholder node: %w", err)
		}
	}

	return domain.EmailConversation{
		ConversationID: plan.ComponentID,
		Method:         domain.ConversationByReferences,
		DuplicateOf:    plan.DuplicateOf,
	}, nil
}

func loadIdentifierNodes(ctx context.Context, tx pgx.Tx, ids []string) (map[string]identifierNode, error) {
	rows, err := tx.Query(ctx, `
        SELECT message_id, COALESCE(doc_id::text, ''), component_id::text
        FROM email_identifier_nodes
        WHERE message_id = ANY($1::text[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("load identifier nodes: %w", err)
	}
	defer rows.Close()

	out := make(map[string]identifierNode, len(ids))
	for rows.Next() {
		var n identifierNode
		if err := rows.Scan(&n.MessageID, &n.DocID, &n.ComponentID); err != nil {
			return nil, fmt.Errorf("load identifier nodes: %w", err)
		}
		out[n.MessageID] = n
	}
	return out, rows.Err()
}

// replaceAddresses rewrites a document's mailboxes. Delete-then-insert, never
// append: a reprocessed message must not accumulate a second copy of its own
// recipients, and a header that lost a mailbox must lose the row too.
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
	// from its Tier 1 bytes and converges on the same rows.
	OnlyMissing bool
}

// EmailDocuments lists the workspace's email message documents in
// deterministic doc_id order.
//
// Only doc_type = 'email' is returned. An archive is a container routed to the
// same worker but is not a message and has no metadata of its own, and a
// document with no Tier 1 object is still returned so the reprocessor can
// report it as unreadable rather than silently losing the missing metadata.
func (r *EmailRepo) EmailDocuments(ctx context.Context, q EmailDocumentQuery) ([]domain.Document, error) {
	rows, err := r.db.Pool.Query(ctx, `
        SELECT d.doc_id::text, d.mime_type, d.rustfs_raw_uri, d.raw_sha256
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
		if err := rows.Scan(&d.DocID, &d.MimeType, &d.RawURI, &d.RawSHA256); err != nil {
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
