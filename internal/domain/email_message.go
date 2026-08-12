// Durable email browse metadata: the structured message model that survives
// parsing and persistence (ingestion-design.md §2.5).
//
// This lives in domain, not in the mail parser, so storage never has to import
// the parser to write a row: the parser produces headers, a mapping layer turns
// them into this, and the repository writes exactly what it is given. Nothing
// here is derived from body text and nothing is fabricated — an absent header
// is an empty value, not a guess.
package domain

import "time"

// EmailAddressKind names the header a mailbox was read from. The set is closed
// because it is a stored discriminator, not free text.
type EmailAddressKind string

const (
	EmailAddressFrom    EmailAddressKind = "from"
	EmailAddressReplyTo EmailAddressKind = "reply_to"
	EmailAddressTo      EmailAddressKind = "to"
	EmailAddressCc      EmailAddressKind = "cc"
	EmailAddressBcc     EmailAddressKind = "bcc"
)

// EmailReferenceKind distinguishes the direct parent candidate from ancestor
// context. Both are stored; only In-Reply-To is an exact parent claim.
type EmailReferenceKind string

const (
	EmailReferenceInReplyTo  EmailReferenceKind = "in_reply_to"
	EmailReferenceReferences EmailReferenceKind = "references"
)

// ConversationMethod records how a message was assigned to a conversation, so
// a caller can tell an exact RFC linkage from a bounded heuristic.
type ConversationMethod string

const (
	// ConversationByReferences means the message carried at least one usable
	// identifier — its own, an In-Reply-To, or a References entry — and was
	// folded into the identifier graph.
	ConversationByReferences ConversationMethod = "references"
	// ConversationBySubject is the labelled fallback for a message with no
	// identifiers at all. It groups by normalized subject and is never
	// presented as an exact conversation.
	ConversationBySubject ConversationMethod = "subject_fallback"
	// ConversationIsolated is a message with neither identifiers nor a
	// subject: it is a conversation of one rather than a member of a bucket
	// keyed on the empty string.
	ConversationIsolated ConversationMethod = "isolated"
)

// EmailAutomatedClass is the stored form of the bounded automated-traffic
// classification. The empty value means ordinary human-authored mail.
type EmailAutomatedClass string

const (
	EmailAutomatedNone          EmailAutomatedClass = ""
	EmailAutomatedList          EmailAutomatedClass = "list"
	EmailAutomatedAutoSubmitted EmailAutomatedClass = "auto_submitted"
	EmailAutomatedBounce        EmailAutomatedClass = "bounce"
)

// WarnDuplicateMessageID is recorded when a second document claims a
// Message-ID another document already owns. The first writer keeps the
// identifier: retargeting it would move an existing conversation onto whichever
// duplicate happened to be ingested last.
const WarnDuplicateMessageID = "duplicate_message_id"

// EmailAddress is one parsed mailbox in one header position.
//
// Address is the normalized exact-match identity and is empty when the mailbox
// could not be parsed; Raw always keeps what was actually written, so evidence
// can render the original and a defect stays inspectable after persistence.
type EmailAddress struct {
	Kind        EmailAddressKind
	Ordinal     int
	Address     string
	DisplayName string
	Raw         string
	Valid       bool
}

// EmailReference is one identifier from In-Reply-To or References, in header
// order. Ordinal preserves that order: References is an ancestor path, and the
// nearest resolvable entry is what parent recovery reads.
type EmailReference struct {
	Kind      EmailReferenceKind
	Ordinal   int
	MessageID string
}

// EmailParseWarning is a stored parse defect. Codes are stable strings so a
// caller can filter them without re-parsing the message.
type EmailParseWarning struct {
	Code   string `json:"code"`
	Header string `json:"header,omitempty"`
	Value  string `json:"value,omitempty"`
}

// EmailParseVersion is the version of the header model these rows were written
// by. It exists so a later parser change can be told apart from a re-ingest of
// the same bytes without re-reading Tier 1.
const EmailParseVersion = 1

// EmailMessage is one email message document's browse metadata.
//
// It carries what the message said about itself. The conversation it belongs to
// is not part of it: that is derived from the workspace's identifier graph at
// write time and returned as an EmailConversation.
type EmailMessage struct {
	DocID string

	// MessageID is the canonical Message-ID, empty when the header was absent
	// or unusable. It is never synthesised.
	MessageID string

	SubjectRaw string
	// SubjectNormalized is the conservative grouping form: lowercased, trimmed,
	// reply and forward prefixes removed.
	SubjectNormalized string

	// SentAt is the parsed Date header, zero when absent or unparsable. It is
	// deliberately distinct from the ingestion timestamp the repository
	// records.
	SentAt time.Time

	AutomatedClass EmailAutomatedClass
	ListID         string

	Addresses  []EmailAddress
	References []EmailReference
	Warnings   []EmailParseWarning
}

// Identifiers is the message's identifier set for conversation assignment: its
// own Message-ID when present, then every In-Reply-To and References entry, in
// header order, deduplicated.
//
// Order matters only for determinism; membership is what folds the message into
// a component. A message that lists its own identifier among its references
// contributes it once, which is why a self-reference cannot form a cycle.
func (m EmailMessage) Identifiers() []string {
	ids := make([]string, 0, 1+len(m.References))
	seen := make(map[string]struct{}, 1+len(m.References))
	add := func(id string) {
		if id == "" {
			return
		}
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	add(m.MessageID)
	for _, r := range m.References {
		add(r.MessageID)
	}
	return ids
}

// PrimarySender is the first valid From mailbox, empty when the message named
// none that could be parsed.
//
// It is the participant the labelled subject fallback groups on. Reply-To is
// deliberately not consulted: it says where a reply should go, not who wrote
// the message, and grouping on it would put one correspondent's mail under
// another's name.
func (m EmailMessage) PrimarySender() string {
	for _, a := range m.Addresses {
		if a.Kind == EmailAddressFrom && a.Valid && a.Address != "" {
			return a.Address
		}
	}
	return ""
}

// EmailConversation is the assignment a write produced: which conversation the
// message landed in, how that was decided, and whether another document already
// owned its Message-ID.
type EmailConversation struct {
	ConversationID string
	Method         ConversationMethod
	// DuplicateOf is the document that already holds this Message-ID, empty
	// when there is none. The identifier stays with that document.
	DuplicateOf string
}
