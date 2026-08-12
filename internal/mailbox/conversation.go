package mailbox

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/suankan/pocket-advisor/internal/domain"
)

// ConversationRequest fetches one conversation by a reference this service
// issued — either for a message or for the conversation itself.
//
// A message reference is accepted because that is what a browse result hands
// back, and "show me the rest of this thread" should not require the caller to
// know that a conversation is a separate identity.
type ConversationRequest struct {
	Ref string
}

// ConversationMessage is one message of a conversation with its derived link
// to its parent.
type ConversationMessage struct {
	ListedMessage
	Relationship Relationship `json:"relationship"`
}

// ConversationResult is one whole conversation, in reading order.
type ConversationResult struct {
	ConversationRef string                `json:"conversation_ref"`
	ConversationID  string                `json:"conversation_id"`
	Method          string                `json:"method"`
	MessageCount    int                   `json:"message_count"`
	Participants    []string              `json:"participants"`
	Messages        []ConversationMessage `json:"messages"`
	Snapshot        time.Time             `json:"snapshot"`
	Omissions       []Omission            `json:"omissions"`
	Warnings        []Warning             `json:"warnings"`
}

// FetchConversation returns every message of one conversation exactly once, in
// stable chronological order, with the relationship each message has to its
// parent.
func (s *Service) FetchConversation(ctx context.Context, req ConversationRequest) (*ConversationResult, error) {
	kind, id, err := decodeRef(req.Ref)
	if err != nil {
		return nil, err
	}

	snapshot, err := s.store.Snapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("read browse snapshot: %w", err)
	}

	conversationID := id
	if kind == refMessage {
		conversationID, err = s.store.ConversationOf(ctx, id)
		if err != nil {
			return nil, err
		}
	}

	msgs, err := s.store.ConversationMessages(ctx, conversationID, snapshot)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, ErrUnknownReference
	}

	// Deduplicate by document before anything reads the set. A conversation is
	// counted, summarised and threaded from these rows, so one message
	// appearing twice would inflate every one of those at once. Attachments and
	// extracted children are not in this set at all: only email message
	// documents have a row in email_messages, so a mail with three attachments
	// is one message here and not four.
	msgs = dedupeByDoc(msgs)
	chronological(msgs)

	omit := newOmissions()
	edges, edgeWarnings, missing := replyEdges(msgs)
	omit.add(OmitMissingAncestor, missing)

	warn := newWarnings()
	for _, w := range edgeWarnings {
		warn.add(w.Code, w.DocID)
	}

	result := &ConversationResult{
		ConversationRef: encodeRef(refConversation, conversationID),
		ConversationID:  conversationID,
		Method:          string(conversationMethod(msgs)),
		MessageCount:    len(msgs),
		Participants:    participantsOf(msgs, s.cfg.MaxParticipants, omit),
		Messages:        make([]ConversationMessage, 0, len(msgs)),
		Snapshot:        snapshot.UTC(),
	}
	for _, m := range msgs {
		for _, w := range m.ParseWarnings {
			warn.add(w.Code, m.DocID)
		}
		result.Messages = append(result.Messages, ConversationMessage{
			ListedMessage: listedMessage(m),
			Relationship:  edges[m.DocID],
		})
	}
	result.Omissions = omit.all()
	result.Warnings = warn.all()
	return result, nil
}

// dedupeByDoc keeps the first row per document, preserving order.
func dedupeByDoc(msgs []Message) []Message {
	seen := make(map[string]struct{}, len(msgs))
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if _, dup := seen[m.DocID]; dup {
			continue
		}
		seen[m.DocID] = struct{}{}
		out = append(out, m)
	}
	return out
}

// conversationMethod reports how this conversation was assigned.
//
// The rows of one conversation share a method by construction — the identifier
// graph and the labelled fallbacks mint disjoint conversation ids — but the
// weakest claim wins if they ever disagree, because presenting a heuristic
// grouping as an exact one is the specific failure this label exists to
// prevent.
func conversationMethod(msgs []Message) domain.ConversationMethod {
	method := domain.ConversationByReferences
	for _, m := range msgs {
		if m.ConversationMethod != "" && m.ConversationMethod != domain.ConversationByReferences {
			return m.ConversationMethod
		}
	}
	return method
}

// participantsOf lists the conversation's distinct senders, ascending, bounded.
func participantsOf(msgs []Message, max int, omit *omissions) []string {
	seen := map[string]struct{}{}
	var all []string
	for _, m := range msgs {
		if m.Sender == "" {
			continue
		}
		if _, dup := seen[m.Sender]; dup {
			continue
		}
		seen[m.Sender] = struct{}{}
		all = append(all, m.Sender)
	}
	sort.Strings(all)
	if len(all) > max {
		omit.add(OmitParticipantsTruncated, len(all)-max)
		all = all[:max]
	}
	return nonNilStrings(all)
}
