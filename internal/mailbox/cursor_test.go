package mailbox

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func testFilters() Filters {
	return Filters{
		Sender:    "ada@example.test",
		Recipient: "owner@example.test",
		After:     at(1, 0),
		Before:    at(9, 0),
	}
}

func TestCursorRoundTripsAKey(t *testing.T) {
	snapshot := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	boundary := key{SentAt: at(4, 9), DocID: docID(3)}

	token, err := cursorFor(boundary, OrderNewestFirst, testFilters(), snapshot)
	if err != nil {
		t.Fatalf("issue cursor: %v", err)
	}
	state, err := decodeCursor(token)
	if err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	if err := state.check(OrderNewestFirst, testFilters()); err != nil {
		t.Fatalf("cursor rejected its own request: %v", err)
	}
	if got := state.key(); !got.SentAt.Equal(boundary.SentAt) || got.DocID != boundary.DocID {
		t.Errorf("key = %v, want %v", got, boundary)
	}
	if !state.Snapshot.Equal(snapshot) {
		t.Errorf("snapshot = %v, want %v", state.Snapshot, snapshot)
	}
}

// A boundary in the undated tail is a position, not a missing value: the next
// page has to resume inside the tail rather than at the top of the order.
func TestCursorCarriesAnUndatedBoundary(t *testing.T) {
	token, err := cursorFor(key{DocID: docID(7)}, OrderNewestFirst, Filters{}, time.Now())
	if err != nil {
		t.Fatalf("issue cursor: %v", err)
	}
	state, err := decodeCursor(token)
	if err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	if state.SentAt != nil {
		t.Errorf("undated boundary encoded a date: %v", state.SentAt)
	}
	if !state.key().undated() || state.key().DocID != docID(7) {
		t.Errorf("key = %v", state.key())
	}
}

// The token is opaque: nothing a caller can read tells them a mailbox was
// filtered on.
func TestCursorDoesNotCarryFilterValues(t *testing.T) {
	token, err := cursorFor(key{DocID: docID(1)}, OrderNewestFirst, testFilters(), time.Now())
	if err != nil {
		t.Fatalf("issue cursor: %v", err)
	}
	if strings.Contains(token, "example") || strings.Contains(token, "ada") {
		t.Errorf("cursor leaks a filter value: %s", token)
	}
}

func TestCursorRejections(t *testing.T) {
	snapshot := time.Now().UTC()
	issued, err := cursorFor(key{SentAt: at(3, 0), DocID: docID(2)}, OrderNewestFirst, testFilters(), snapshot)
	if err != nil {
		t.Fatalf("issue cursor: %v", err)
	}

	changed := testFilters()
	changed.Sender = "bob@example.test"
	widened := testFilters()
	widened.After = time.Time{}
	collapsed := testFilters()
	collapsed.Collapse = true

	for _, tc := range []struct {
		name    string
		cursor  string
		order   Order
		filters Filters
		want    error
	}{
		{"not base64", "not a cursor!!", OrderNewestFirst, testFilters(), ErrCursorMalformed},
		{"not json", "aGVsbG8", OrderNewestFirst, testFilters(), ErrCursorMalformed},
		{"empty", "", OrderNewestFirst, testFilters(), ErrCursorMalformed},
		{"other order", issued, OrderOldestFirst, testFilters(), ErrCursorOrder},
		{"other sender", issued, OrderNewestFirst, changed, ErrCursorFilters},
		{"widened range", issued, OrderNewestFirst, widened, ErrCursorFilters},
		{"collapse toggled", issued, OrderNewestFirst, collapsed, ErrCursorFilters},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, err := decodeCursor(tc.cursor)
			if err == nil {
				err = state.check(tc.order, tc.filters)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), "@") {
				t.Errorf("rejection echoed an address: %v", err)
			}
		})
	}
}

// A cursor minted by another version describes a page layout this build may no
// longer produce, so it is refused rather than reinterpreted.
func TestCursorRejectsAnotherVersion(t *testing.T) {
	state := cursorState{
		Version:     cursorVersion + 1,
		Order:       OrderNewestFirst,
		Fingerprint: Filters{}.fingerprint(),
		DocID:       docID(1),
		Snapshot:    time.Now().UTC(),
	}
	token, err := encodeCursor(state)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := decodeCursor(token); !errors.Is(err, ErrCursorVersion) {
		t.Fatalf("err = %v, want ErrCursorVersion", err)
	}
}

// A doc_id that is not the canonical lowercase form would order differently in
// Go and in PostgreSQL, so it never becomes a page boundary.
func TestCursorRejectsANonCanonicalKey(t *testing.T) {
	state := cursorState{
		Version:     cursorVersion,
		Order:       OrderNewestFirst,
		Fingerprint: Filters{}.fingerprint(),
		DocID:       "00000000-0000-4000-8000-00000000000A",
		Snapshot:    time.Now().UTC(),
	}
	token, err := encodeCursor(state)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := decodeCursor(token); !errors.Is(err, ErrCursorMalformed) {
		t.Fatalf("err = %v, want ErrCursorMalformed", err)
	}
}

func TestFilterFingerprintSeparatesEveryField(t *testing.T) {
	base := testFilters()
	variants := map[string]Filters{
		"sender":    {Sender: "bob@example.test", Recipient: base.Recipient, After: base.After, Before: base.Before},
		"recipient": {Sender: base.Sender, Recipient: "carol@example.test", After: base.After, Before: base.Before},
		"after":     {Sender: base.Sender, Recipient: base.Recipient, After: at(2, 0), Before: base.Before},
		"before":    {Sender: base.Sender, Recipient: base.Recipient, After: base.After, Before: at(10, 0)},
		"collapse":  {Sender: base.Sender, Recipient: base.Recipient, After: base.After, Before: base.Before, Collapse: true},
	}
	for name, v := range variants {
		if v.fingerprint() == base.fingerprint() {
			t.Errorf("%s change did not alter the fingerprint", name)
		}
	}
	if base.fingerprint() != testFilters().fingerprint() {
		t.Error("identical filters produced different fingerprints")
	}
}

// Length prefixing is what stops two different filter sets from digesting the
// same bytes.
func TestFilterFingerprintIsUnambiguous(t *testing.T) {
	a := Filters{Sender: "ab@x.test", Recipient: "c@x.test"}
	b := Filters{Sender: "ab@x.testc@x.test"}
	if a.fingerprint() == b.fingerprint() {
		t.Error("concatenated filter values collided")
	}
}

// The fingerprint is a mismatch detector, not a store of what was filtered on.
func TestFilterFingerprintHidesItsInput(t *testing.T) {
	f := testFilters()
	if strings.Contains(f.fingerprint(), "ada") || strings.Contains(f.fingerprint(), "example") {
		t.Errorf("fingerprint leaks its input: %s", f.fingerprint())
	}
}
