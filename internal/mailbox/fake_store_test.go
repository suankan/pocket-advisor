package mailbox

import (
	"context"
	"sort"
	"time"

	"github.com/suankan/pocket-advisor/internal/domain"
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
	workspace string
	messages  []Message
	// now advances only when a test says so, so "ingested after the snapshot"
	// is a deliberate condition rather than a race.
	now time.Time
}

func newFakeStore(workspace string) *fakeStore {
	return &fakeStore{workspace: workspace, now: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)}
}

// ingest stores a message as if a worker had just written it.
func (f *fakeStore) ingest(m Message) {
	f.now = f.now.Add(time.Second)
	m.IngestedAt = f.now
	f.messages = append(f.messages, m)
}

func (f *fakeStore) Snapshot(context.Context) (time.Time, error) { return f.now, nil }

func (f *fakeStore) ListMessages(_ context.Context, q PageQuery) ([]Message, error) {
	var matched []Message
	for _, m := range f.messages {
		if q.WorkspaceID != f.workspace {
			continue
		}
		if m.IngestedAt.After(q.Snapshot) || !matches(m, q.Filters) {
			continue
		}
		matched = append(matched, m)
	}

	counts := map[string]int{}
	for _, m := range matched {
		counts[m.ConversationID]++
	}
	for i := range matched {
		matched[i].ConversationMatches = counts[matched[i].ConversationID]
	}
	sortByOrder(matched, q.Order)

	if q.Filters.Collapse {
		seen := map[string]struct{}{}
		var reps []Message
		for _, m := range matched {
			if _, dup := seen[m.ConversationID]; dup {
				continue
			}
			seen[m.ConversationID] = struct{}{}
			reps = append(reps, m)
		}
		matched = reps
	}

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

func matches(m Message, f Filters) bool {
	if f.Sender != "" && m.Sender != f.Sender {
		return false
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

func (f *fakeStore) Summaries(_ context.Context, workspaceID string, ids []string, snapshot time.Time) (map[string]Aggregate, error) {
	want := map[string]struct{}{}
	for _, id := range ids {
		want[id] = struct{}{}
	}
	out := map[string]Aggregate{}
	for _, m := range f.messages {
		if workspaceID != f.workspace || m.IngestedAt.After(snapshot) {
			continue
		}
		if _, ok := want[m.ConversationID]; !ok {
			continue
		}
		agg := out[m.ConversationID]
		agg.ConversationID = m.ConversationID
		agg.Method = m.ConversationMethod
		agg.MessageCount++
		if !m.SentAt.IsZero() {
			if agg.FirstSentAt.IsZero() || m.SentAt.Before(agg.FirstSentAt) {
				agg.FirstSentAt = m.SentAt
			}
			if m.SentAt.After(agg.LastSentAt) {
				agg.LastSentAt = m.SentAt
			}
		}
		if m.Sender != "" {
			agg.Participants = appendDistinct(agg.Participants, m.Sender)
		}
		out[m.ConversationID] = agg
	}
	for id, agg := range out {
		sort.Strings(agg.Participants)
		out[id] = agg
	}
	return out, nil
}

func (f *fakeStore) ConversationOf(_ context.Context, workspaceID, docID string) (string, error) {
	for _, m := range f.messages {
		if workspaceID == f.workspace && m.DocID == docID {
			return m.ConversationID, nil
		}
	}
	return "", ErrUnknownReference
}

func (f *fakeStore) ConversationMessages(_ context.Context, workspaceID, conversationID string, snapshot time.Time) ([]Message, error) {
	var msgs []Message
	for _, m := range f.messages {
		if workspaceID != f.workspace || m.ConversationID != conversationID {
			continue
		}
		if m.IngestedAt.After(snapshot) {
			continue
		}
		msgs = append(msgs, m)
	}
	if len(msgs) == 0 {
		return nil, ErrUnknownReference
	}
	chronological(msgs)
	return msgs, nil
}

// ---- synthetic fixtures ----------------------------------------------------
//
// Every address is under .test, which cannot name a real mailbox, and every
// identifier is invented here.

const testWorkspace = "mailbox-unit"

// docID mints a canonical identifier from a counter, so tests can name the
// document they mean without depending on a hash.
func docID(n int) string {
	const hex = "0123456789abcdef"
	b := []byte("00000000-0000-4000-8000-000000000000")
	b[len(b)-1] = hex[n%16]
	b[len(b)-2] = hex[(n/16)%16]
	return string(b)
}

func conversationID(n int) string {
	const hex = "0123456789abcdef"
	b := []byte("11111111-0000-4000-8000-000000000000")
	b[len(b)-1] = hex[n%16]
	return string(b)
}

// synthetic builds one message. sentAt of the zero value means the Date header
// was missing or unparsable.
func synthetic(n int, conversation int, sender string, sentAt time.Time) Message {
	return Message{
		DocID:              docID(n),
		MessageID:          "m" + docID(n) + "@mail.example.test",
		ConversationID:     conversationID(conversation),
		ConversationMethod: domain.ConversationByReferences,
		Subject:            "Synthetic subject",
		SentAt:             sentAt,
		Sender:             sender,
		Recipients:         []string{"owner@example.test"},
	}
}

func at(day, hour int) time.Time {
	return time.Date(2026, 1, day, hour, 0, 0, 0, time.UTC)
}
