package mailbox

import (
	"context"
	"strings"
	"time"
)

// fakeStore is an in-memory Store.
//
// It exists so the decisions this package actually owns — page boundaries,
// collapse accounting, reply-edge derivation, cursor discipline — can be tested
// against enumerated inputs rather than against a database. It deliberately
// mirrors the PostgreSQL store's semantics through the same Order helpers the
// SQL is generated from; the database-backed tests in mailbox_manual_test.go
// are what prove the SQL agrees.
type fakeStore struct {
	messages []Message
	// now advances only when a test says so, so "ingested after the snapshot"
	// is a deliberate condition rather than a race.
	now time.Time
}

func newFakeStore() *fakeStore {
	return &fakeStore{now: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)}
}

// ingest stores a message as if a worker had just written it.
func (f *fakeStore) ingest(m Message) {
	f.now = f.now.Add(time.Second)
	m.IngestedAt = f.now
	f.messages = append(f.messages, m)
}

func (f *fakeStore) ListMessages(_ context.Context, q PageQuery) ([]Message, error) {
	var matched []Message
	for _, m := range f.messages {
		if m.IngestedAt.After(q.Snapshot) || !matches(m, q.Filters) || !matchesDirection(m, q.Filters.Direction, q.OwnerIdentities) {
			continue
		}
		matched = append(matched, m)
	}
	sortByOrder(matched, q.Order)

	var page []Message
	for _, m := range matched {
		if q.After != nil && !q.Order.afterKey(*q.After, m.key()) {
			continue
		}
		page = append(page, m)
		if len(page) == q.Limit {
			break
		}
	}
	return page, nil
}

func (f *fakeStore) Snapshot(context.Context) (time.Time, error) { return f.now, nil }

func matches(m Message, f Filters) bool {
	if f.Sender != "" && m.Sender != f.Sender {
		return false
	}
	if f.SenderDomain != "" {
		at := strings.LastIndex(m.Sender, "@")
		if at < 0 || m.Sender[at+1:] != f.SenderDomain {
			return false
		}
	}
	if f.Recipient != "" {
		found := false
		for _, r := range m.Recipients {
			if r == f.Recipient {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if !f.After.IsZero() && (m.SentAt.IsZero() || m.SentAt.Before(f.After)) {
		return false
	}
	if !f.Before.IsZero() && (m.SentAt.IsZero() || !m.SentAt.Before(f.Before)) {
		return false
	}
	return true
}

func matchesDirection(m Message, direction Direction, owners []string) bool {
	if direction == "" || direction == DirectionEither {
		return true
	}
	isOwner := func(address string) bool {
		for _, owner := range owners {
			if address == owner {
				return true
			}
		}
		return false
	}
	fromOwner := isOwner(m.Sender)
	toOwner := false
	for _, recipient := range m.Recipients {
		if isOwner(recipient) {
			toOwner = true
			break
		}
	}
	if direction == DirectionOutbound {
		return fromOwner
	}
	return toOwner && !fromOwner
}

// ---- synthetic fixtures ----------------------------------------------------
//
// Every address is under .test, which cannot name a real mailbox, and every
// identifier is invented here.

// docID mints a canonical identifier from a counter, so tests can name the
// document they mean without depending on a hash.
func docID(n int) string {
	const hex = "0123456789abcdef"
	b := []byte("00000000-0000-4000-8000-000000000000")
	b[len(b)-1] = hex[n%16]
	b[len(b)-2] = hex[(n/16)%16]
	return string(b)
}

// synthetic builds one message. sentAt of the zero value means the Date header
// was missing or unparsable.
func synthetic(n int, _ int, sender string, sentAt time.Time) Message {
	return Message{
		DocID:      docID(n),
		MessageID:  "m" + docID(n) + "@mail.example.test",
		Subject:    "Synthetic subject",
		SentAt:     sentAt,
		Sender:     sender,
		Recipients: []string{"owner@example.test"},
	}
}

func at(day, hour int) time.Time {
	return time.Date(2026, 1, day, hour, 0, 0, 0, time.UTC)
}
