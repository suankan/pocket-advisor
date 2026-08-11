package mailbox

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/workspace"
)

// WarnUndatedCandidateChronology explains that a message without a usable Date
// header was placed after dated messages by the immutable document-id tie
// breaker. It is not a claim that the message was actually sent last.
const WarnUndatedCandidateChronology = "undated_candidate_chronology"

// AwaitingReplyRequest bounds the deterministic review set. Participant and
// date bounds apply to the candidate inbound message, not to later events: an
// owner reply outside a requested date range must still close the candidate.
type AwaitingReplyRequest struct {
	Participant string
	After       time.Time
	Before      time.Time
	Limit       int
}

// AwaitingReplyFilters is the normalized scope that selected candidates.
type AwaitingReplyFilters struct {
	Participant string     `json:"participant,omitempty"`
	After       *time.Time `json:"after,omitempty"`
	Before      *time.Time `json:"before,omitempty"`
}

// CandidateEvent retains an event after the latest inbound evidence. Automated
// events remain visible and labelled rather than being silently treated as a
// human reply.
type CandidateEvent struct {
	ListedMessage
	Relationship Relationship `json:"relationship"`
}

// AwaitingReplyCandidate is evidence for a possible outstanding reply, not an
// action recommendation. LatestInbound is the exact source message that made
// it a candidate; LaterEvents lets a caller assess subsequent automated or
// third-party activity without assuming it answers the owner.
type AwaitingReplyCandidate struct {
	ConversationRef    string           `json:"conversation_ref"`
	ConversationID     string           `json:"conversation_id"`
	ConversationMethod string           `json:"conversation_method"`
	LatestInbound      CandidateEvent   `json:"latest_inbound"`
	LaterEvents        []CandidateEvent `json:"later_events"`
	Participants       []string         `json:"participants"`
	FirstSentAt        *time.Time       `json:"first_sent_at,omitempty"`
	LastSentAt         *time.Time       `json:"last_sent_at,omitempty"`
	Warnings           []Warning        `json:"warnings"`
}

// AwaitingReplyResult is a bounded, deterministic candidate review set.
type AwaitingReplyResult struct {
	Candidates []AwaitingReplyCandidate `json:"candidates"`
	Filters    AwaitingReplyFilters     `json:"filters"`
	Limit      int                      `json:"limit"`
	Snapshot   time.Time                `json:"snapshot"`
	Omissions  []Omission               `json:"omissions"`
	Warnings   []Warning                `json:"warnings"`
}

// AwaitingReplyCandidates finds the latest relevant human inbound event in an
// exact-reference conversation and retains it only when no later outbound
// owner event is an exact reply descendant. Subject grouping, embeddings and
// any other similarity signal are deliberately absent from this operation.
func (s *Service) AwaitingReplyCandidates(ctx context.Context, req AwaitingReplyRequest) (*AwaitingReplyResult, error) {
	if err := s.requireOwnerIdentities(); err != nil {
		return nil, err
	}
	filters, err := normalizeAwaitingReplyFilters(req)
	if err != nil {
		return nil, err
	}
	limit := req.Limit
	warnings := newWarnings()
	if limit <= 0 {
		limit = s.cfg.DefaultLimit
	}
	if limit > s.cfg.MaxCandidates {
		limit = s.cfg.MaxCandidates
		warnings.add(WarnLimitClamped, "")
	}

	snapshot, err := s.store.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	messages, err := s.store.CandidateMessages(ctx, CandidateQuery{
		WorkspaceID: s.workspace, OwnerIdentities: s.ownerIdentities,
		Participant: filters.Participant, After: dereferenceTime(filters.After),
		Before: dereferenceTime(filters.Before), Snapshot: snapshot,
	})
	if err != nil {
		return nil, err
	}
	byConversation := make(map[string][]Message)
	for _, m := range messages {
		// A store must never turn a labelled subject fallback into proof of a
		// reply merely because it selected the same subject bucket.
		if m.ConversationMethod != domain.ConversationByReferences {
			continue
		}
		byConversation[m.ConversationID] = append(byConversation[m.ConversationID], m)
	}

	candidates := make([]AwaitingReplyCandidate, 0, len(byConversation))
	for _, messages := range byConversation {
		candidate, ok := s.awaitingCandidate(messages, filters)
		if !ok {
			continue
		}
		candidates = append(candidates, candidate)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return OrderNewestFirst.before(candidateKey(candidates[i]), candidateKey(candidates[j]))
	})
	omit := newOmissions()
	if len(candidates) > limit {
		omit.add("awaiting_reply_candidates", len(candidates)-limit)
		candidates = candidates[:limit]
	}
	for _, candidate := range candidates {
		for _, warning := range candidate.Warnings {
			warnings.add(warning.Code, warning.DocID)
		}
	}
	return &AwaitingReplyResult{Candidates: candidates, Filters: filters, Limit: limit,
		Snapshot: snapshot.UTC(), Omissions: omit.all(), Warnings: warnings.all()}, nil
}

func normalizeAwaitingReplyFilters(req AwaitingReplyRequest) (AwaitingReplyFilters, error) {
	f := AwaitingReplyFilters{}
	if !req.After.IsZero() {
		after := req.After.UTC()
		f.After = &after
	}
	if !req.Before.IsZero() {
		before := req.Before.UTC()
		f.Before = &before
	}
	if f.After != nil && f.Before != nil && !f.After.Before(*f.Before) {
		return f, errors.New("date range is empty: after must precede before")
	}
	if req.Participant != "" {
		address, err := workspace.NormalizeMailbox(req.Participant)
		if err != nil {
			return f, errors.New("participant filter is not a single valid mailbox")
		}
		f.Participant = address
	}
	return f, nil
}

func dereferenceTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func (s *Service) awaitingCandidate(messages []Message, filters AwaitingReplyFilters) (AwaitingReplyCandidate, bool) {
	messages = dedupeByDoc(messages)
	chronological(messages)
	edges, edgeWarnings, _ := replyEdges(messages)

	inbound := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if s.isRelevantInbound(messages[i]) && candidateMatches(messages[i], filters) {
			inbound = i
			break
		}
	}
	if inbound < 0 {
		return AwaitingReplyCandidate{}, false
	}

	// A later owner message closes the candidate only if its exact parent path
	// reaches this inbound message. A same-subject mail and even an unrelated
	// branch of the same reference component are not proof of a reply.
	for i := inbound + 1; i < len(messages); i++ {
		if s.isOutboundOwner(messages[i]) && descendsFrom(messages[i].DocID, messages[inbound].DocID, edges) {
			return AwaitingReplyCandidate{}, false
		}
	}

	warn := newWarnings()
	for _, warning := range edgeWarnings {
		warn.add(warning.Code, warning.DocID)
	}
	for _, m := range messages {
		for _, warning := range m.ParseWarnings {
			warn.add(warning.Code, m.DocID)
		}
	}
	if messages[inbound].SentAt.IsZero() {
		warn.add(WarnUndatedCandidateChronology, messages[inbound].DocID)
	}

	later := make([]CandidateEvent, 0, len(messages)-inbound-1)
	for _, m := range messages[inbound+1:] {
		if m.SentAt.IsZero() {
			warn.add(WarnUndatedCandidateChronology, m.DocID)
		}
		later = append(later, CandidateEvent{ListedMessage: listedMessage(m), Relationship: edges[m.DocID]})
	}
	return AwaitingReplyCandidate{
		ConversationRef:    encodeRef(refConversation, messages[inbound].ConversationID),
		ConversationID:     messages[inbound].ConversationID,
		ConversationMethod: string(conversationMethod(messages)),
		LatestInbound:      CandidateEvent{ListedMessage: listedMessage(messages[inbound]), Relationship: edges[messages[inbound].DocID]},
		LaterEvents:        later, Participants: allParticipants(messages),
		FirstSentAt: optionalTime(messages[0].SentAt), LastSentAt: optionalTime(messages[len(messages)-1].SentAt),
		Warnings: warn.all(),
	}, true
}

func candidateKey(c AwaitingReplyCandidate) key {
	return key{SentAt: dereferenceTime(c.LatestInbound.SentAt), DocID: c.LatestInbound.DocID}
}

func (s *Service) isRelevantInbound(m Message) bool {
	if m.AutomatedClass != domain.EmailAutomatedNone || s.isOwner(m.Sender) {
		return false
	}
	for _, recipient := range m.Recipients {
		if s.isOwner(recipient) {
			return true
		}
	}
	return false
}

func (s *Service) isOutboundOwner(m Message) bool {
	return m.AutomatedClass == domain.EmailAutomatedNone && s.isOwner(m.Sender)
}

func (s *Service) isOwner(address string) bool {
	for _, owner := range s.ownerIdentities {
		if address == owner {
			return true
		}
	}
	return false
}

func candidateMatches(m Message, filters AwaitingReplyFilters) bool {
	if filters.Participant != "" {
		matched := m.Sender == filters.Participant
		for _, recipient := range m.Recipients {
			matched = matched || recipient == filters.Participant
		}
		if !matched {
			return false
		}
	}
	if filters.After != nil && (m.SentAt.IsZero() || m.SentAt.Before(*filters.After)) {
		return false
	}
	return filters.Before == nil || (!m.SentAt.IsZero() && m.SentAt.Before(*filters.Before))
}

// descendsFrom follows only relationships produced from stored Message-ID
// headers. There is no conversation-id or subject shortcut in this traversal.
func descendsFrom(docID, ancestor string, edges map[string]Relationship) bool {
	seen := map[string]struct{}{}
	for docID != "" {
		if docID == ancestor {
			return true
		}
		if _, cycle := seen[docID]; cycle {
			return false
		}
		seen[docID] = struct{}{}
		docID = edges[docID].ParentDocID
	}
	return false
}

func allParticipants(messages []Message) []string {
	seen := map[string]struct{}{}
	for _, m := range messages {
		if m.Sender != "" {
			seen[m.Sender] = struct{}{}
		}
		for _, recipient := range m.Recipients {
			if recipient != "" {
				seen[recipient] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for participant := range seen {
		out = append(out, participant)
	}
	sort.Strings(out)
	return nonNilStrings(out)
}
