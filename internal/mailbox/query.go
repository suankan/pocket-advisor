package mailbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/suankan/pocket-advisor/internal/workspace"
)

// Filters is the complete browse predicate. Every field is a value, never an
// expression: exact normalized mailboxes, half-open date bounds, and a flag.
type Filters struct {
	// Sender matches the message's From mailboxes exactly, after
	// normalization. A display name is not an identity and is not matched.
	Sender string
	// Recipient matches To, Cc, or Bcc. One field rather than three because
	// "was this person written to" does not usually distinguish the header
	// they were written to in; the distinction survives in the returned rows.
	Recipient string
	// After bounds the parsed Date inclusively, Before exclusively, so
	// consecutive windows tile without overlapping or dropping a message.
	After  time.Time
	Before time.Time
	// Collapse returns one row per conversation — the matching message that
	// is extreme in the requested order — plus a summary of the conversation.
	Collapse bool
}

func (f Filters) dated() bool { return !f.After.IsZero() || !f.Before.IsZero() }

// ListRequest is what a caller asks for. No transport types, and no workspace:
// scope belongs to the Service.
type ListRequest struct {
	Sender    string
	Recipient string
	After     time.Time
	Before    time.Time
	Order     Order
	Limit     int
	// CollapseConversations returns the matched message plus conversation
	// summary fields instead of every matching message.
	CollapseConversations bool
	// Cursor continues a previous page series. It must have been issued for
	// the same filters and order.
	Cursor string
}

// AppliedFilters echoes what the service actually used, normalized. A caller
// that sent "Ada Adviser <ADA@Example.test>" needs to see the mailbox that was
// matched, or an empty result is unexplainable.
type AppliedFilters struct {
	Sender    string     `json:"sender,omitempty"`
	Recipient string     `json:"recipient,omitempty"`
	After     *time.Time `json:"after,omitempty"`
	Before    *time.Time `json:"before,omitempty"`
	Collapse  bool       `json:"collapse_conversations"`
}

// Page is the pagination state of one result.
type Page struct {
	Limit      int    `json:"limit"`
	Returned   int    `json:"returned"`
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor,omitempty"`
	// Snapshot is the ingestion watermark this series is being read at.
	// Messages ingested after it appear only in a new series.
	Snapshot time.Time `json:"snapshot"`
}

// ListedMessage is one message in a browse result.
type ListedMessage struct {
	Ref                string     `json:"ref"`
	DocID              string     `json:"doc_id"`
	ConversationRef    string     `json:"conversation_ref"`
	ConversationID     string     `json:"conversation_id"`
	ConversationMethod string     `json:"conversation_method"`
	Subject            string     `json:"subject"`
	SentAt             *time.Time `json:"sent_at"`
	Sender             string     `json:"sender"`
	Recipients         []string   `json:"recipients"`
	AutomatedClass     string     `json:"automated_class,omitempty"`
	ListID             string     `json:"list_id,omitempty"`

	// Conversation is present only under collapse.
	Conversation *ConversationSummary `json:"conversation,omitempty"`
}

// ConversationSummary describes the conversation a collapsed row stands for.
type ConversationSummary struct {
	ConversationRef string     `json:"conversation_ref"`
	ConversationID  string     `json:"conversation_id"`
	Method          string     `json:"method"`
	MessageCount    int        `json:"message_count"`
	MatchedCount    int        `json:"matched_count"`
	FirstSentAt     *time.Time `json:"first_sent_at"`
	LastSentAt      *time.Time `json:"last_sent_at"`
	Participants    []string   `json:"participants"`
}

// ListResult is the deliverable of a browse query.
type ListResult struct {
	Messages  []ListedMessage `json:"messages"`
	Filters   AppliedFilters  `json:"filters"`
	Order     Order           `json:"order"`
	Page      Page            `json:"page"`
	Omissions []Omission      `json:"omissions"`
	Warnings  []Warning       `json:"warnings"`
}

// ListMessages returns one page of messages.
//
// The shape of the work: normalize the request into filters the store can only
// match exactly, resolve the page series (a fresh snapshot, or the one the
// cursor was issued against), ask for one row more than requested so "is there
// more" needs no second query, then decorate what came back.
func (s *Service) ListMessages(ctx context.Context, req ListRequest) (*ListResult, error) {
	filters, order, err := s.normalize(req)
	if err != nil {
		return nil, err
	}

	warn := newWarnings()
	omit := newOmissions()

	limit := req.Limit
	switch {
	case limit <= 0:
		limit = s.cfg.DefaultLimit
	case limit > s.cfg.MaxLimit:
		limit = s.cfg.MaxLimit
		warn.add(WarnLimitClamped, "")
	}
	// A date bound and an unparsable Date header cannot both be honoured. Say
	// so rather than letting a caller conclude the messages do not exist.
	if filters.dated() {
		warn.add(WarnUndatedExcluded, "")
	}

	var (
		snapshot time.Time
		after    *key
	)
	if req.Cursor != "" {
		state, err := decodeCursor(req.Cursor)
		if err != nil {
			return nil, err
		}
		if err := state.check(order, filters); err != nil {
			return nil, err
		}
		snapshot = state.Snapshot
		boundary := state.key()
		after = &boundary
	} else {
		snapshot, err = s.store.Snapshot(ctx)
		if err != nil {
			return nil, fmt.Errorf("read browse snapshot: %w", err)
		}
	}

	rows, err := s.store.ListMessages(ctx, PageQuery{
		WorkspaceID: s.workspace,
		Filters:     filters,
		Order:       order,
		// One extra row decides HasMore. A count would answer a different
		// question — how many match now — which is not what the caller is
		// paging through.
		Limit:    limit + 1,
		Snapshot: snapshot,
		After:    after,
	})
	if err != nil {
		return nil, err
	}
	if err := verifyPage(rows, order, after); err != nil {
		return nil, err
	}

	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}

	result := &ListResult{
		Filters: appliedFilters(filters),
		Order:   order,
		Page: Page{
			Limit:    limit,
			Returned: len(rows),
			HasMore:  hasMore,
			Snapshot: snapshot.UTC(),
		},
	}

	summaries := map[string]Aggregate{}
	if filters.Collapse && len(rows) > 0 {
		ids := make([]string, 0, len(rows))
		for _, m := range rows {
			ids = append(ids, m.ConversationID)
		}
		summaries, err = s.store.Summaries(ctx, s.workspace, ids, snapshot)
		if err != nil {
			return nil, err
		}
	}

	result.Messages = make([]ListedMessage, 0, len(rows))
	for _, m := range rows {
		listed := listedMessage(m)
		for _, w := range m.ParseWarnings {
			warn.add(w.Code, m.DocID)
		}
		if filters.Collapse {
			// Every message beyond the representative is a match the caller
			// asked not to see, which is an omission with a known size.
			if m.ConversationMatches > 1 {
				omit.add(OmitCollapsedMessages, m.ConversationMatches-1)
			}
			agg, ok := summaries[m.ConversationID]
			if !ok {
				// The conversation lost its rows between the page and the
				// summary. Report the message rather than dropping it.
				agg = Aggregate{ConversationID: m.ConversationID, Method: m.ConversationMethod}
			}
			summary, dropped := s.summarize(agg, m.ConversationMatches)
			omit.add(OmitParticipantsTruncated, dropped)
			listed.Conversation = &summary
		}
		result.Messages = append(result.Messages, listed)
	}

	if hasMore && len(rows) > 0 {
		cursor, err := cursorFor(rows[len(rows)-1].key(), order, filters, snapshot)
		if err != nil {
			return nil, fmt.Errorf("issue pagination cursor: %w", err)
		}
		result.Page.NextCursor = cursor
	}

	result.Omissions = omit.all()
	result.Warnings = warn.all()
	return result, nil
}

// normalize turns a request into the exact-match filter set and validates the
// order.
//
// Mailboxes go through the same normalization the workspace registry and the
// ingestion path use, so a header, a configured owner identity and a query
// argument are all compared in one form. An unusable filter is refused rather
// than silently matching nothing — and the error never repeats the value,
// because a rejected filter is exactly the string most likely to be a real
// mailbox.
func (s *Service) normalize(req ListRequest) (Filters, Order, error) {
	order := req.Order
	if order == "" {
		order = OrderNewestFirst
	}
	if !order.valid() {
		return Filters{}, "", errUnsupportedOrder(order)
	}

	f := Filters{
		After:    req.After.UTC(),
		Before:   req.Before.UTC(),
		Collapse: req.CollapseConversations,
	}
	if req.After.IsZero() {
		f.After = time.Time{}
	}
	if req.Before.IsZero() {
		f.Before = time.Time{}
	}
	if !f.After.IsZero() && !f.Before.IsZero() && !f.After.Before(f.Before) {
		return Filters{}, "", errors.New("date range is empty: after must precede before")
	}

	if req.Sender != "" {
		addr, err := workspace.NormalizeMailbox(req.Sender)
		if err != nil {
			return Filters{}, "", errors.New("sender filter is not a single valid mailbox")
		}
		f.Sender = addr
	}
	if req.Recipient != "" {
		addr, err := workspace.NormalizeMailbox(req.Recipient)
		if err != nil {
			return Filters{}, "", errors.New("recipient filter is not a single valid mailbox")
		}
		f.Recipient = addr
	}
	return f, order, nil
}

// verifyPage refuses a page that is not strictly ordered and strictly after the
// boundary it was asked for.
//
// This is a store contract check, not defensive decoration. A page that repeats
// the boundary row, or that comes back in the wrong order, is precisely the
// failure that makes pagination silently duplicate or skip messages — the one
// class of bug a caller cannot detect from the outside.
func verifyPage(rows []Message, order Order, after *key) error {
	for i, m := range rows {
		if after != nil && !order.afterKey(*after, m.key()) {
			return fmt.Errorf("store returned a row at or before the page boundary")
		}
		if i > 0 && !order.before(rows[i-1].key(), m.key()) {
			return fmt.Errorf("store returned rows out of %s order", order)
		}
	}
	return nil
}

func appliedFilters(f Filters) AppliedFilters {
	a := AppliedFilters{Sender: f.Sender, Recipient: f.Recipient, Collapse: f.Collapse}
	if !f.After.IsZero() {
		after := f.After.UTC()
		a.After = &after
	}
	if !f.Before.IsZero() {
		before := f.Before.UTC()
		a.Before = &before
	}
	return a
}

func listedMessage(m Message) ListedMessage {
	listed := ListedMessage{
		Ref:                encodeRef(refMessage, m.DocID),
		DocID:              m.DocID,
		ConversationRef:    encodeRef(refConversation, m.ConversationID),
		ConversationID:     m.ConversationID,
		ConversationMethod: string(m.ConversationMethod),
		Subject:            m.Subject,
		SentAt:             optionalTime(m.SentAt),
		Sender:             m.Sender,
		Recipients:         nonNilStrings(m.Recipients),
		AutomatedClass:     string(m.AutomatedClass),
		ListID:             m.ListID,
	}
	return listed
}

// summarize bounds a conversation summary and reports how many participants it
// had to leave out.
func (s *Service) summarize(agg Aggregate, matched int) (ConversationSummary, int) {
	participants := agg.Participants
	dropped := 0
	if len(participants) > s.cfg.MaxParticipants {
		dropped = len(participants) - s.cfg.MaxParticipants
		participants = participants[:s.cfg.MaxParticipants]
	}
	return ConversationSummary{
		ConversationRef: encodeRef(refConversation, agg.ConversationID),
		ConversationID:  agg.ConversationID,
		Method:          string(agg.Method),
		MessageCount:    agg.MessageCount,
		MatchedCount:    matched,
		FirstSentAt:     optionalTime(agg.FirstSentAt),
		LastSentAt:      optionalTime(agg.LastSentAt),
		Participants:    nonNilStrings(participants),
	}, dropped
}

func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	utc := t.UTC()
	return &utc
}
