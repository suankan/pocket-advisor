package mailbox

import (
	"sort"
	"time"
)

// Ordering and the pagination key.
//
// One sort key serves the whole browse path: (sent_at, doc_id). The date alone
// is not a key — several messages routinely share a timestamp to the second,
// and a page boundary drawn on a non-unique value either repeats or skips the
// rows sharing it — so the document identifier is the tiebreak. It is derived
// and immutable (domain.NewDocID), which is what makes a boundary reproducible
// after the row it names has been re-ingested.

// Order is the browse sort direction.
type Order string

const (
	// OrderNewestFirst is sent_at DESC NULLS LAST, doc_id DESC. Undated
	// messages sort last: a message whose Date header was absent must remain
	// reachable, but it cannot be claimed to be the newest.
	OrderNewestFirst Order = "newest_first"
	// OrderOldestFirst is the exact reverse — sent_at ASC NULLS FIRST, doc_id
	// ASC — so the two orders enumerate the same rows in opposite directions.
	// Any other spelling of "oldest first" (NULLS LAST, say) would place the
	// undated messages at the same end in both directions, and the two orders
	// would no longer be reverses of one another.
	OrderOldestFirst Order = "oldest_first"
)

func (o Order) valid() bool {
	return o == OrderNewestFirst || o == OrderOldestFirst
}

// key is one row's position in the browse order.
type key struct {
	// SentAt is the parsed Date header; the zero value means the message
	// carried none, which is a position in the order, not a missing key.
	SentAt time.Time
	DocID  string
}

func (k key) undated() bool { return k.SentAt.IsZero() }

// before reports whether a sorts strictly before b under o.
//
// Written once for newest-first and mirrored for oldest-first rather than
// spelled out twice: the reversal is the definition of the second order, so
// deriving it removes the possibility of the two drifting apart.
func (o Order) before(a, b key) bool {
	if o == OrderOldestFirst {
		return newestFirstBefore(b, a)
	}
	return newestFirstBefore(a, b)
}

func newestFirstBefore(a, b key) bool {
	switch {
	case a.undated() && b.undated():
		return a.DocID > b.DocID
	case a.undated():
		return false
	case b.undated():
		return true
	case !a.SentAt.Equal(b.SentAt):
		return a.SentAt.After(b.SentAt)
	default:
		return a.DocID > b.DocID
	}
}

// afterKey reports whether k belongs strictly after the page boundary prev.
//
// This is the executable statement of the SQL in keysetPredicate. The two are
// held together by the database-backed tests, which page the same fixtures
// through both; keeping the Go form lets the service verify that a store
// actually returned the page it asked for instead of trusting it.
func (o Order) afterKey(prev, k key) bool { return o.before(prev, k) }

// sortByOrder puts messages into the browse order.
func sortByOrder(msgs []Message, o Order) {
	sort.SliceStable(msgs, func(i, j int) bool {
		return o.before(msgs[i].key(), msgs[j].key())
	})
}

// chronological is the conversation order: oldest first, undated last,
// doc_id ascending.
//
// Deliberately not OrderOldestFirst. A browse page is a window that has to be
// reversible, so its undated tail sits at whichever end reversal puts it. A
// conversation is a story being read: an undated message belongs at the end,
// where it is visibly unplaced, not at the front where it would look like the
// message that started the thread.
func chronological(msgs []Message) {
	sort.SliceStable(msgs, func(i, j int) bool {
		a, b := msgs[i].key(), msgs[j].key()
		switch {
		case a.undated() && b.undated():
			return a.DocID < b.DocID
		case a.undated():
			return false
		case b.undated():
			return true
		case !a.SentAt.Equal(b.SentAt):
			return a.SentAt.Before(b.SentAt)
		default:
			return a.DocID < b.DocID
		}
	})
}
