package mailbox

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/suankan/pocket-advisor/internal/source"
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
// source_path is the original file's location relative to the workspace's
// local staging directory (ingestion-design.md's Provenance.SourcePath,
// stored in documents.metadata_headers); every caller of this projection
// joins in the owning documents row to supply it.
const messageColumns = `
    m.doc_id::text, m.message_id, m.subject_raw, m.sent_at, m.ingested_at,
    m.automated_class,
    m.list_id, m.parse_warnings::text,
    COALESCE(d.metadata_headers->>'source_path', '') AS source_path`

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

	args := []any{q.Snapshot}
	var where strings.Builder
	// The snapshot bound is not an optimisation. Without it a page taken
	// minutes after the cursor was issued would include messages ingested in
	// between, which is how a paginated read starts repeating and skipping
	// rows while a worker is running.
	where.WriteString("m.ingested_at <= $1")

	if q.Filters.Sender != "" {
		args = append(args, q.Filters.Sender)
		fmt.Fprintf(&where, `
        AND EXISTS (SELECT 1 FROM email_addresses a
                    WHERE a.doc_id = m.doc_id AND a.kind = 'from'
                      AND a.valid AND a.address = $%d)`, len(args))
	}
	if q.Filters.SenderDomain != "" {
		args = append(args, q.Filters.SenderDomain)
		// The stored address is normalized but may have a quoted local part.
		// Splitting the reversed value finds the domain after its final @ rather
		// than assuming every local part is simple. The matching expression has
		// a partial index in the schema for bounded organization browsing.
		fmt.Fprintf(&where, `
        AND EXISTS (SELECT 1 FROM email_addresses a
                    WHERE a.doc_id = m.doc_id AND a.kind = 'from'
                      AND a.valid
                      AND reverse(split_part(reverse(a.address), '@', 1)) = $%d)`, len(args))
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
    SELECT %s
    FROM email_messages m
    JOIN documents d ON d.doc_id = m.doc_id
    WHERE %s
)`, messageColumns, where.String())

	fmt.Fprintf(&sql, `
SELECT doc_id, message_id, subject_raw, sent_at, ingested_at,
       automated_class, list_id, parse_warnings, source_path
FROM matched`)

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
	if err := p.hydrateContainerPaths(ctx, msgs); err != nil {
		return nil, err
	}
	if err := p.markSharedIdentifiers(ctx, msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

// markSharedIdentifiers reports the messages on this page whose Message-ID is
// also carried by another document.
//
// Derived when read rather than decided when written. Nothing elects an owner
// for a repeated identifier any more, so "is this identifier shared" is a
// question about the corpus as it stands now: a second copy ingested later
// makes the first one shared too, which a warning frozen into the first
// writer's row could never say. The message_id index answers it directly, and
// only for the rows on this page.
func (p *PostgresStore) markSharedIdentifiers(ctx context.Context, msgs []Message) error {
	ids := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if m.MessageID != "" {
			ids = appendDistinct(ids, m.MessageID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := p.db.Pool.Query(ctx, `
        SELECT message_id FROM email_messages
        WHERE message_id = ANY($1::text[])
        GROUP BY message_id HAVING count(*) > 1`, ids)
	if err != nil {
		return fmt.Errorf("detect shared message identifiers: %w", err)
	}
	defer rows.Close()

	shared := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("detect shared message identifiers: %w", err)
		}
		shared[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("detect shared message identifiers: %w", err)
	}
	for i := range msgs {
		if _, dup := shared[msgs[i].MessageID]; !dup {
			continue
		}
		// Header and value stay empty: a Message-ID is workspace material and
		// the warning travels to a client. The code alone says what is wrong.
		msgs[i].ParseWarnings = append(msgs[i].ParseWarnings,
			domain.EmailParseWarning{Code: domain.WarnDuplicateMessageID})
	}
	return nil
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

func scanMessages(rows pgx.Rows) ([]Message, error) {
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var (
			m          Message
			sentAt     *time.Time
			automated  string
			warningsJS string
		)
		if err := rows.Scan(
			&m.DocID, &m.MessageID, &m.Subject, &sentAt, &m.IngestedAt,
			&automated, &m.ListID, &warningsJS, &m.SourcePath,
		); err != nil {
			return nil, fmt.Errorf("read message row: %w", err)
		}
		if sentAt != nil {
			m.SentAt = *sentAt
		}
		m.AutomatedClass = domain.EmailAutomatedClass(automated)
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

// hydrateContainerPaths fills ContainerPath for the rows that have no staged
// file of their own — a message nested inside another message or an archive.
//
// A second round trip rather than a join, for the same reason hydrateAddresses
// is one: the walk is recursive and per-document, and folding it into the page
// query would multiply the keyset the limit is applied to. Rows that already
// carry SourcePath are skipped, so a page of ordinary staged mail issues no
// query at all.
func (p *PostgresStore) hydrateContainerPaths(ctx context.Context, msgs []Message) error {
	locations, err := postgres.NewSourceRepo(p.db).Locations(ctx, docIDs(msgs))
	if err != nil {
		return err
	}
	for i := range msgs {
		msgs[i].SourceLocations = locations[msgs[i].DocID]
		msgs[i].SourcePath, msgs[i].ContainerPath = source.DisplayLocations(msgs[i].SourceLocations)
	}
	return nil
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
