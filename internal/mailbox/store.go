package mailbox

import (
	"context"
	"time"

	"github.com/suankan/pocket-advisor/internal/domain"
)

// Store is the row source this package reads.
//
// It exists as an interface for one reason: the parts of browsing that are easy
// to get wrong — page boundaries, reply-edge derivation, collapse accounting —
// are pure decisions over rows, and they are worth testing without a database
// in the way. The PostgreSQL implementation lives in store_postgres.go and is
// exercised by the manual, database-backed tests.
//
// Every method takes the workspace explicitly, and the Service is the only
// caller: it passes its own fixed scope, which no request can reach.
type Store interface {
	// Snapshot returns the ingestion watermark a new page series is taken
	// against. It comes from the store rather than the process clock because
	// it is compared against ingested_at, which the database writes.
	Snapshot(ctx context.Context) (time.Time, error)

	// ListMessages returns up to q.Limit rows matching q, in q.Order, starting
	// strictly after q.After when it is set. Rows ingested after q.Snapshot
	// are never returned.
	ListMessages(ctx context.Context, q PageQuery) ([]Message, error)

	// Summaries aggregates whole conversations for the collapse view. The
	// snapshot applies here too: a summary must describe the same set of
	// messages the page was drawn from.
	Summaries(ctx context.Context, workspaceID string, conversationIDs []string, snapshot time.Time) (map[string]Aggregate, error)

	// ConversationOf resolves one message document to its conversation.
	// Returns ErrUnknownReference when the workspace holds no such message.
	ConversationOf(ctx context.Context, workspaceID, docID string) (string, error)

	// ConversationMessages returns every message of a conversation in
	// chronological order. Returns ErrUnknownReference for a conversation with
	// no messages in scope.
	ConversationMessages(ctx context.Context, workspaceID, conversationID string, snapshot time.Time) ([]Message, error)
}

// PageQuery is one page request as the store receives it.
//
// There is no free-text predicate and no ordering argument beyond the closed
// Order type: everything the store can be asked to do is enumerated here, which
// is what makes "callers cannot inject a filter expression" a property of the
// type rather than of the input validation.
type PageQuery struct {
	WorkspaceID string
	Filters     Filters
	Order       Order
	Limit       int
	Snapshot    time.Time
	// After is the exclusive page boundary, nil for the first page.
	After *key
}

// Message is one stored email message as browsing sees it.
//
// It carries the message's own metadata plus the two derived values a browse
// result needs: the conversation it was assigned to, and — under collapse — how
// many messages of that conversation matched the filters.
type Message struct {
	DocID          string
	MessageID      string
	ConversationID string
	// ConversationMethod is how the message was assigned to its conversation
	// at write time: an exact identifier component, the labelled subject
	// fallback, or an isolated message.
	ConversationMethod domain.ConversationMethod

	Subject    string
	SentAt     time.Time
	IngestedAt time.Time

	Sender     string
	Recipients []string

	AutomatedClass domain.EmailAutomatedClass
	ListID         string

	// References are the stored In-Reply-To and References identifiers in
	// header order. Reply edges are derived from these at read time.
	References []domain.EmailReference
	// ParseWarnings are the defects recorded when the message was parsed.
	// Surfaced as warning codes only: their header values may hold addresses.
	ParseWarnings []domain.EmailParseWarning

	// ConversationMatches is how many messages of this conversation matched
	// the filters, set only when the store collapsed conversations.
	ConversationMatches int
}

func (m Message) key() key { return key{SentAt: m.SentAt, DocID: m.DocID} }

// Aggregate is a whole conversation summarised, independent of which of its
// messages matched.
type Aggregate struct {
	ConversationID string
	Method         domain.ConversationMethod
	MessageCount   int
	FirstSentAt    time.Time
	LastSentAt     time.Time
	// Participants are the distinct normalized From mailboxes, ascending. The
	// caller bounds how many it renders; the store returns what it found.
	Participants []string
}
