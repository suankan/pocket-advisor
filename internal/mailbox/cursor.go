package mailbox

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Stable pagination.
//
// A cursor is an opaque continuation token, not a page number. It carries the
// exact position the previous page ended at, the filters and order it was
// issued for, and the snapshot watermark that page was taken against. Together
// those give the only property that matters: paging through a live mailbox
// returns every matching message exactly once, whatever is ingested while the
// caller is reading.
//
// The watermark is what makes that true. Offsets shift when a row is inserted
// ahead of the current position, and a keyset alone still admits messages that
// arrive with an older Date than the page boundary — a mailbox is backfilled
// out of order all the time. Fixing ingested_at at the first page means later
// arrivals are simply not in the set being paged; the caller sees them by
// starting a new series.

// cursorVersion is the encoding version. It is checked rather than guessed at:
// a cursor from an older build describes a page layout this one may no longer
// produce, and silently reinterpreting it would repeat or skip messages.
const cursorVersion = 2

// cursorState is what a cursor encodes. Field names are short because the
// encoded form travels through every transport above this package.
type cursorState struct {
	Version     int    `json:"v"`
	Order       Order  `json:"o"`
	Fingerprint string `json:"f"`
	// SentAt is nil when the boundary row is undated — a real position in the
	// order, not a missing value.
	SentAt   *time.Time `json:"k,omitempty"`
	DocID    string     `json:"d"`
	Snapshot time.Time  `json:"w"`
}

// Cursor rejection reasons. Each is a distinct condition a caller can act on,
// and none of them echoes a filter value back: an error string is the most
// likely part of a result to reach a log collector.
var (
	ErrCursorMalformed = errors.New("pagination cursor is not readable")
	ErrCursorVersion   = errors.New("pagination cursor was issued by a different version of this service")
	ErrCursorOrder     = errors.New("pagination cursor was issued for a different sort order")
	ErrCursorFilters   = errors.New("pagination cursor was issued for different filters")
)

func encodeCursor(c cursorState) (string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeCursor(s string) (cursorState, error) {
	var c cursorState
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return c, ErrCursorMalformed
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, ErrCursorMalformed
	}
	if c.Version != cursorVersion {
		return c, ErrCursorVersion
	}
	if !c.Order.valid() || c.DocID == "" || c.Snapshot.IsZero() || c.Fingerprint == "" {
		return c, ErrCursorMalformed
	}
	if !isUUID(c.DocID) {
		return c, ErrCursorMalformed
	}
	return c, nil
}

// check refuses a cursor that does not belong to this request.
//
// Continuing a page series under changed filters or a changed order is not a
// smaller mistake than a corrupt cursor: the boundary key means nothing in a
// different sequence, so the caller would silently receive an arbitrary
// sub-range of a set they never asked for. It is rejected rather than
// reinterpreted, and the fix — start a new series — is the caller's to make.
func (c cursorState) check(o Order, f Filters) error {
	if c.Order != o {
		return ErrCursorOrder
	}
	if c.Fingerprint != f.fingerprint() {
		return ErrCursorFilters
	}
	return nil
}

func (c cursorState) key() key {
	k := key{DocID: c.DocID}
	if c.SentAt != nil {
		k.SentAt = *c.SentAt
	}
	return k
}

func cursorFor(k key, o Order, f Filters, snapshot time.Time) (string, error) {
	state := cursorState{
		Version:     cursorVersion,
		Order:       o,
		Fingerprint: f.fingerprint(),
		DocID:       k.DocID,
		Snapshot:    snapshot.UTC(),
	}
	if !k.undated() {
		sent := k.SentAt.UTC()
		state.SentAt = &sent
	}
	return encodeCursor(state)
}

// fingerprint identifies the filter set a cursor was issued for.
//
// A hash rather than the values themselves, for two reasons. It keeps mailbox
// addresses out of a token that will be logged, copied, and pasted into issue
// reports by whatever sits above this package; and it makes the comparison
// exact by construction — every field is in the digest, so a filter added later
// cannot be forgotten here and silently permit a mismatched continuation.
//
// The encoding is length-prefixed so that no combination of field values can
// be mistaken for another: without it, sender "a@x" with recipient "b@x" and
// sender "a@xb@x" with no recipient would digest the same bytes.
func (f Filters) fingerprint() string {
	h := sha256.New()
	write := func(s string) {
		h.Write([]byte(strconv.Itoa(len(s))))
		h.Write([]byte(":"))
		h.Write([]byte(s))
	}
	write("v" + strconv.Itoa(cursorVersion))
	write(f.Sender)
	write(f.SenderDomain)
	write(f.Recipient)
	write(string(f.Direction))
	write(timeFingerprint(f.After))
	write(timeFingerprint(f.Before))
	if f.Collapse {
		write("collapse")
	} else {
		write("expanded")
	}
	// Half the digest: this is a mismatch detector between two values produced
	// by the same process, not a signature.
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func timeFingerprint(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
