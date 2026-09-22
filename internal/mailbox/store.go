package mailbox

import (
	"context"
	"github.com/suankan/pocket-advisor/internal/source"
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
// Every workspace is its own database (deviation 34), so no method here takes
// a workspace argument: the Store is already connected to the one database a
// request could ever reach, and a parameter repeating that would be a
// predicate that is always true.
type Store interface {
	// Snapshot returns the ingestion watermark a new page series is taken
	// against. It comes from the store rather than the process clock because
	// it is compared against ingested_at, which the database writes.
	Snapshot(ctx context.Context) (time.Time, error)

	// ListMessages returns up to q.Limit rows matching q, in q.Order, starting
	// strictly after q.After when it is set. Rows ingested after q.Snapshot
	// are never returned.
	ListMessages(ctx context.Context, q PageQuery) ([]Message, error)

	// snapshot applies here too: a summary must describe the same set of
	// messages the page was drawn from.

	// at least one potentially eligible inbound human message. It returns every
	// event in those conversations so a later reply cannot be hidden by a date
	// or participant filter on the candidate itself.

	// Returns ErrUnknownReference when the workspace holds no such message.

	// chronological order. Returns ErrUnknownReference for a conversation with
	// no messages in scope.
}

// PageQuery is one page request as the store receives it.
//
// There is no free-text predicate and no ordering argument beyond the closed
// Order type: everything the store can be asked to do is enumerated here, which
// is what makes "callers cannot inject a filter expression" a property of the
// type rather than of the input validation.
type PageQuery struct {
	Filters  Filters
	Order    Order
	Limit    int
	Snapshot time.Time
	// After is the exclusive page boundary, nil for the first page.
	After *key
	// OwnerIdentities is set only for an explicit direction filter.
	OwnerIdentities []string
}

// Message is one email row as the browse result needs it.
type Message struct {
	DocID      string
	MessageID  string
	Subject    string
	SentAt     time.Time
	IngestedAt time.Time

	Sender     string
	Recipients []string

	AutomatedClass domain.EmailAutomatedClass
	ListID         string

	// SourcePath is the message's original file location relative to the
	// workspace's local source directory, exactly as the scan recorded
	// it (ingestion-design.md's Provenance.SourcePath). Empty for a message
	// that was never a staged file of its own — one nested inside another
	// message or an archive — which carries ContainerPath instead.
	SourceLocations []source.Location
	SourcePath      string
	// ContainerPath is the staged file this message was extracted from,
	// resolved as the nearest ancestor holding a path. It is set exactly
	// when SourcePath is empty, so every message can state where it lives
	// without a caller inferring one.
	ContainerPath string

	// References are the stored In-Reply-To and References identifiers in
	// header order. Reply edges are derived from these at read time.
	References []domain.EmailReference
	// ParseWarnings are the defects recorded when the message was parsed.
	// Surfaced as warning codes only: their header values may hold addresses.
	ParseWarnings []domain.EmailParseWarning
}

func (m Message) key() key { return key{SentAt: m.SentAt, DocID: m.DocID} }
