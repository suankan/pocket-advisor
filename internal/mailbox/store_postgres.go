package mailbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
)

// The PostgreSQL browse store.
//
// The SQL lives with the read path that owns it, as the retrieval package's
// fusion query does, because these statements are the query plan of this
// feature rather than a general-purpose repository: the keyset predicate, the
// snapshot bound and the collapse are one design, and splitting them across
// packages would let them drift.
//
// Every statement is built from closed alternatives — a fixed column list, an
// order chosen from two, predicates that exist or do not — and every value is a
// bound parameter. No caller-supplied text is ever concatenated into SQL.

// PostgresStore reads the email browse tables (ingestion-design.md §2.5).
type PostgresStore struct{ db *postgres.DB }

// NewPostgresStore wires the store to an already-open Tier 2 pool.
func NewPostgresStore(db *postgres.DB) *PostgresStore { return &PostgresStore{db: db} }

// messageColumns is the projection every message read shares. Explicit and
// identical everywhere, so a scan cannot silently bind to a different column.
const messageColumns = `
    m.doc_id::text, m.message_id, m.subject_raw, m.sent_at, m.ingested_at,
    m.conversation_id::text, m.conversation_method, m.automated_class,
    m.list_id, m.parse_warnings::text`

// Snapshot reads the watermark from the database clock, which is the clock
// ingested_at is written by. A process clock would drift against it and either
// hide rows that were already stored or admit rows written after the page.
func (p *PostgresStore) Snapshot(ctx context.Context) (time.Time, error) {
	var now time.Time
	if err := p.db.Pool.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("read database time: %w", err)
	}
	return now, nil
}

// ListMessages runs one page of the browse query.
func (p *PostgresStore) ListMessages(ctx context.Context, q PageQuery) ([]Message, error) {
	if !q.Order.valid() {
		return nil, errUnsupportedOrder(q.Order)
	}

	args := []any{q.WorkspaceID, q.Snapshot}
	var where strings.Builder
	// The snapshot bound is not an optimisation. Without it a page taken
	// minutes after the cursor was issued would include messages ingested in
	// between, which is how a paginated read starts repeating and skipping
	// rows while a worker is running.
	where.WriteString("m.workspace_id = $1 AND m.ingested_at <= $2")

	if q.Filters.Sender != "" {
		args = append(args, q.Filters.Sender)
		fmt.Fprintf(&where, `
        AND EXISTS (SELECT 1 FROM email_addresses a
                    WHERE a.doc_id = m.doc_id AND a.kind = 'from'
                      AND a.valid AND a.address = $%d)`, len(args))
	}
	if q.Filters.Recipient != "" {
		// One predicate over the three recipient headers: a person written to
		// on Cc was still written to, and a caller asking "did I hear from
		// them" should not have to ask three times.
		args = append(args, q.Filters.Recipient)
		fmt.Fprintf(&where, `
        AND EXISTS (SELECT 1 FROM email_addresses a
                    WHERE a.doc_id = m.doc_id AND a.kind IN ('to','cc','bcc')
                      AND a.valid AND a.address = $%d)`, len(args))
	}
	if q.Filters.Direction != "" {
		if len(q.OwnerIdentities) == 0 {
			return nil, ErrOwnerIdentitiesRequired
		}
		args = append(args, q.OwnerIdentities)
		owners := fmt.Sprintf("$%d::text[]", len(args))
		switch q.Filters.Direction {
		case DirectionOutbound:
			fmt.Fprintf(&where, `
        AND EXISTS (SELECT 1 FROM email_addresses a
                    WHERE a.doc_id = m.doc_id AND a.kind = 'from'
                      AND a.valid AND a.address = ANY(%s))`, owners)
		case DirectionInbound:
			// Mail from one owner alias to another is owner-authored, never
			// inbound merely because the receiving alias was also configured.
			fmt.Fprintf(&where, `
        AND EXISTS (SELECT 1 FROM email_addresses a
                    WHERE a.doc_id = m.doc_id AND a.kind IN ('to','cc','bcc')
                      AND a.valid AND a.address = ANY(%s))
        AND NOT EXISTS (SELECT 1 FROM email_addresses a
                        WHERE a.doc_id = m.doc_id AND a.kind = 'from'
                          AND a.valid AND a.address = ANY(%s))`, owners, owners)
		case DirectionEither:
			// Explicit either documents that owner-relative direction was
			// requested but intentionally admits both sides.
		default:
			return nil, fmt.Errorf("unsupported message direction %q", q.Filters.Direction)
		}
	}
	// Half-open, matching Filters: >= after, < before. Undated messages fail
	// both comparisons and drop out, which the service reports as a warning
	// rather than leaving the caller to infer.
	if !q.Filters.After.IsZero() {
		args = append(args, q.Filters.After)
		fmt.Fprintf(&where, " AND m.sent_at >= $%d", len(args))
	}
	if !q.Filters.Before.IsZero() {
		args = append(args, q.Filters.Before)
		fmt.Fprintf(&where, " AND m.sent_at < $%d", len(args))
	}

	var sql strings.Builder
	fmt.Fprintf(&sql, `
WITH matched AS (
    SELECT %s,
           count(*) OVER (PARTITION BY m.conversation_id) AS conversation_matches
    FROM email_messages m
    WHERE %s
)`, messageColumns, where.String())

	source := "matched"
	if q.Filters.Collapse {
		// DISTINCT ON picks each conversation's representative over the whole
		// matched set, not over the page, so the collapsed sequence is a
		// deterministic subsequence of the expanded one — which is what lets
		// the same keyset cursor page it. The representative is the extreme
		// matching message in the requested order: newest-first collapses to
		// the latest matching message of each conversation.
		fmt.Fprintf(&sql, `,
representatives AS (
    SELECT DISTINCT ON (conversation_id) * FROM matched
    ORDER BY conversation_id, %s
)`, orderSQL(q.Order))
		source = "representatives"
	}

	fmt.Fprintf(&sql, `
SELECT doc_id, message_id, subject_raw, sent_at, ingested_at, conversation_id,
       conversation_method, automated_class, list_id, parse_warnings,
       conversation_matches
FROM %s`, source)

	if q.After != nil {
		args = append(args, q.After.DocID)
		docArg := fmt.Sprintf("$%d", len(args))
		sentArg := ""
		if !q.After.undated() {
			args = append(args, q.After.SentAt)
			sentArg = fmt.Sprintf("$%d", len(args))
		}
		fmt.Fprintf(&sql, "\nWHERE (%s)", keysetPredicate(q.Order, *q.After, sentArg, docArg))
	}

	args = append(args, q.Limit)
	fmt.Fprintf(&sql, "\nORDER BY %s\nLIMIT $%d", orderSQL(q.Order), len(args))

	rows, err := p.db.Pool.Query(ctx, sql.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("browse messages: %w", err)
	}
	msgs, err := scanMessages(rows)
	if err != nil {
		return nil, err
	}
	if err := p.hydrateAddresses(ctx, msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

// orderSQL renders the browse order. DESC NULLS LAST and its exact reverse,
// matching email_messages_keyset_idx so a page is an index range scan.
func orderSQL(o Order) string {
	if o == OrderOldestFirst {
		return "sent_at ASC NULLS FIRST, doc_id ASC"
	}
	return "sent_at DESC NULLS LAST, doc_id DESC"
}

// keysetPredicate renders "strictly after this page boundary".
//
// The undated cases are separate statements rather than a clever expression
// because NULL comparisons are not orderable: once the newest-first page has
// reached the undated tail, everything remaining is undated with a smaller
// doc_id, and nothing else can follow. Order.afterKey states the same rule in
// Go and the database-backed tests page identical fixtures through both.
//
// sentArg is empty exactly when the boundary row is undated.
func keysetPredicate(o Order, k key, sentArg, docArg string) string {
	if o == OrderOldestFirst {
		if k.undated() {
			// Nulls sort first here, so the dated remainder all follows.
			return fmt.Sprintf("(sent_at IS NULL AND doc_id > %s) OR sent_at IS NOT NULL", docArg)
		}
		return fmt.Sprintf(
			"sent_at IS NOT NULL AND (sent_at > %s OR (sent_at = %s AND doc_id > %s))",
			sentArg, sentArg, docArg)
	}
	if k.undated() {
		return fmt.Sprintf("sent_at IS NULL AND doc_id < %s", docArg)
	}
	return fmt.Sprintf(
		"sent_at IS NULL OR sent_at < %s OR (sent_at = %s AND doc_id < %s)",
		sentArg, sentArg, docArg)
}

// Summaries aggregates whole conversations, under the same snapshot the page
// was drawn at.
func (p *PostgresStore) Summaries(ctx context.Context, workspaceID string, conversationIDs []string, snapshot time.Time) (map[string]Aggregate, error) {
	ids := distinct(conversationIDs)
	out := make(map[string]Aggregate, len(ids))
	if len(ids) == 0 {
		return out, nil
	}

	rows, err := p.db.Pool.Query(ctx, `
        SELECT conversation_id::text, min(conversation_method), count(*),
               min(sent_at), max(sent_at)
        FROM email_messages
        WHERE workspace_id = $1 AND ingested_at <= $2 AND conversation_id = ANY($3::uuid[])
        GROUP BY conversation_id`, workspaceID, snapshot, ids)
	if err != nil {
		return nil, fmt.Errorf("summarize conversations: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			agg    Aggregate
			method string
			first  *time.Time
			last   *time.Time
		)
		if err := rows.Scan(&agg.ConversationID, &method, &agg.MessageCount, &first, &last); err != nil {
			return nil, fmt.Errorf("summarize conversations: %w", err)
		}
		agg.Method = domain.ConversationMethod(method)
		if first != nil {
			agg.FirstSentAt = *first
		}
		if last != nil {
			agg.LastSentAt = *last
		}
		out[agg.ConversationID] = agg
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("summarize conversations: %w", err)
	}

	parts, err := p.db.Pool.Query(ctx, `
        SELECT DISTINCT m.conversation_id::text, a.address
        FROM email_messages m
        JOIN email_addresses a ON a.doc_id = m.doc_id
        WHERE m.workspace_id = $1 AND m.ingested_at <= $2
          AND m.conversation_id = ANY($3::uuid[])
          AND a.kind = 'from' AND a.valid AND a.address <> ''
        ORDER BY 1, 2`, workspaceID, snapshot, ids)
	if err != nil {
		return nil, fmt.Errorf("summarize participants: %w", err)
	}
	defer parts.Close()

	for parts.Next() {
		var conversationID, address string
		if err := parts.Scan(&conversationID, &address); err != nil {
			return nil, fmt.Errorf("summarize participants: %w", err)
		}
		agg, ok := out[conversationID]
		if !ok {
			continue
		}
		agg.Participants = append(agg.Participants, address)
		out[conversationID] = agg
	}
	if err := parts.Err(); err != nil {
		return nil, fmt.Errorf("summarize participants: %w", err)
	}
	return out, nil
}

// CandidateMessages selects exact-reference conversations through an eligible
// inbound human event, then returns every event in those selected conversations.
// The second step deliberately has no participant/date predicate: a reply just
// outside a review window still changes whether its inbound message awaits one.
func (p *PostgresStore) CandidateMessages(ctx context.Context, q CandidateQuery) ([]Message, error) {
	if len(q.OwnerIdentities) == 0 {
		return nil, ErrOwnerIdentitiesRequired
	}
	args := []any{q.WorkspaceID, q.Snapshot, q.OwnerIdentities}
	var eligible strings.Builder
	eligible.WriteString(`m.workspace_id = $1 AND m.ingested_at <= $2
          AND m.conversation_method = 'references' AND m.automated_class = ''
          AND EXISTS (SELECT 1 FROM email_addresses a
                      WHERE a.doc_id = m.doc_id AND a.kind IN ('to','cc','bcc')
                        AND a.valid AND a.address = ANY($3::text[]))
          AND NOT EXISTS (SELECT 1 FROM email_addresses a
                          WHERE a.doc_id = m.doc_id AND a.kind = 'from'
                            AND a.valid AND a.address = ANY($3::text[]))`)
	if q.Participant != "" {
		args = append(args, q.Participant)
		fmt.Fprintf(&eligible, ` AND EXISTS (SELECT 1 FROM email_addresses a
                      WHERE a.doc_id = m.doc_id AND a.valid AND a.address = $%d)`, len(args))
	}
	if !q.After.IsZero() {
		args = append(args, q.After)
		fmt.Fprintf(&eligible, " AND m.sent_at >= $%d", len(args))
	}
	if !q.Before.IsZero() {
		args = append(args, q.Before)
		fmt.Fprintf(&eligible, " AND m.sent_at < $%d", len(args))
	}
	query := fmt.Sprintf(`
WITH eligible_conversations AS (
    SELECT DISTINCT m.conversation_id FROM email_messages m WHERE %s
)
SELECT %s, 0
FROM email_messages m
JOIN eligible_conversations e ON e.conversation_id = m.conversation_id
WHERE m.workspace_id = $1 AND m.ingested_at <= $2
ORDER BY m.conversation_id, m.sent_at ASC NULLS LAST, m.doc_id ASC`, eligible.String(), messageColumns)
	rows, err := p.db.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read awaiting-reply candidates: %w", err)
	}
	msgs, err := scanMessages(rows)
	if err != nil {
		return nil, err
	}
	if err := p.hydrateAddresses(ctx, msgs); err != nil {
		return nil, err
	}
	if err := p.hydrateReferences(ctx, msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

// ConversationOf resolves a message document to its conversation.
func (p *PostgresStore) ConversationOf(ctx context.Context, workspaceID, docID string) (string, error) {
	var conversationID string
	err := p.db.Pool.QueryRow(ctx, `
        SELECT conversation_id::text FROM email_messages
        WHERE workspace_id = $1 AND doc_id = $2::uuid`, workspaceID, docID).Scan(&conversationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrUnknownReference
	}
	if err != nil {
		return "", fmt.Errorf("resolve message reference: %w", err)
	}
	return conversationID, nil
}

// ConversationMessages returns a conversation in chronological order.
//
// Chronology is the database's, not the caller's: sent_at ASC NULLS LAST with
// doc_id ASC as the tiebreak, so an undated message sits at the end where it is
// visibly unplaced rather than at the front pretending to have started the
// thread.
func (p *PostgresStore) ConversationMessages(ctx context.Context, workspaceID, conversationID string, snapshot time.Time) ([]Message, error) {
	rows, err := p.db.Pool.Query(ctx, fmt.Sprintf(`
        SELECT %s, 0
        FROM email_messages m
        WHERE m.workspace_id = $1 AND m.conversation_id = $2::uuid AND m.ingested_at <= $3
        ORDER BY m.sent_at ASC NULLS LAST, m.doc_id ASC`, messageColumns), workspaceID, conversationID, snapshot)
	if err != nil {
		return nil, fmt.Errorf("read conversation: %w", err)
	}
	msgs, err := scanMessages(rows)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, ErrUnknownReference
	}
	if err := p.hydrateAddresses(ctx, msgs); err != nil {
		return nil, err
	}
	if err := p.hydrateReferences(ctx, msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

func scanMessages(rows pgx.Rows) ([]Message, error) {
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var (
			m          Message
			sentAt     *time.Time
			method     string
			automated  string
			warningsJS string
			matches    int
		)
		if err := rows.Scan(
			&m.DocID, &m.MessageID, &m.Subject, &sentAt, &m.IngestedAt,
			&m.ConversationID, &method, &automated, &m.ListID, &warningsJS,
			&matches,
		); err != nil {
			return nil, fmt.Errorf("read message row: %w", err)
		}
		if sentAt != nil {
			m.SentAt = *sentAt
		}
		m.ConversationMethod = domain.ConversationMethod(method)
		m.AutomatedClass = domain.EmailAutomatedClass(automated)
		m.ConversationMatches = matches
		if warningsJS != "" {
			if err := json.Unmarshal([]byte(warningsJS), &m.ParseWarnings); err != nil {
				return nil, fmt.Errorf("read message %s parse warnings: %w", m.DocID, err)
			}
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read message rows: %w", err)
	}
	return out, nil
}

// hydrateAddresses attaches the sender and recipients of the rows on this page.
//
// A second round trip rather than a join: joining the mailbox rows onto the
// page multiplies every message by its recipient count, and paging a limit over
// a multiplied row set is exactly the mistake that returns half a page.
func (p *PostgresStore) hydrateAddresses(ctx context.Context, msgs []Message) error {
	if len(msgs) == 0 {
		return nil
	}
	ids := docIDs(msgs)
	rows, err := p.db.Pool.Query(ctx, `
        SELECT doc_id::text, kind, address FROM email_addresses
        WHERE doc_id = ANY($1::uuid[]) AND valid AND address <> ''
        ORDER BY doc_id, kind, ordinal`, ids)
	if err != nil {
		return fmt.Errorf("read message addresses: %w", err)
	}
	defer rows.Close()

	senders := map[string]string{}
	recipients := map[string][]string{}
	for rows.Next() {
		var docID, kind, address string
		if err := rows.Scan(&docID, &kind, &address); err != nil {
			return fmt.Errorf("read message addresses: %w", err)
		}
		switch domain.EmailAddressKind(kind) {
		case domain.EmailAddressFrom:
			// First From mailbox wins; Reply-To is deliberately not a sender.
			if _, ok := senders[docID]; !ok {
				senders[docID] = address
			}
		case domain.EmailAddressTo, domain.EmailAddressCc, domain.EmailAddressBcc:
			recipients[docID] = appendDistinct(recipients[docID], address)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read message addresses: %w", err)
	}

	for i := range msgs {
		msgs[i].Sender = senders[msgs[i].DocID]
		msgs[i].Recipients = recipients[msgs[i].DocID]
	}
	return nil
}

// hydrateReferences attaches the stored reply headers, which is what reply-edge
// derivation reads.
func (p *PostgresStore) hydrateReferences(ctx context.Context, msgs []Message) error {
	if len(msgs) == 0 {
		return nil
	}
	rows, err := p.db.Pool.Query(ctx, `
        SELECT doc_id::text, kind, ordinal, message_id FROM email_references
        WHERE doc_id = ANY($1::uuid[])
        ORDER BY doc_id, kind, ordinal`, docIDs(msgs))
	if err != nil {
		return fmt.Errorf("read reply headers: %w", err)
	}
	defer rows.Close()

	refs := map[string][]domain.EmailReference{}
	for rows.Next() {
		var (
			docID string
			ref   domain.EmailReference
			kind  string
		)
		if err := rows.Scan(&docID, &kind, &ref.Ordinal, &ref.MessageID); err != nil {
			return fmt.Errorf("read reply headers: %w", err)
		}
		ref.Kind = domain.EmailReferenceKind(kind)
		refs[docID] = append(refs[docID], ref)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read reply headers: %w", err)
	}

	for i := range msgs {
		msgs[i].References = refs[msgs[i].DocID]
	}
	return nil
}

func docIDs(msgs []Message) []string {
	ids := make([]string, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.DocID)
	}
	return ids
}

func distinct(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v == "" {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func appendDistinct(values []string, v string) []string {
	for _, existing := range values {
		if existing == v {
			return values
		}
	}
	return append(values, v)
}
