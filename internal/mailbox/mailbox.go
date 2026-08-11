// Package mailbox is the exact email browse and conversation read path: list
// messages by mailbox and date, and fetch one conversation whole.
//
// It is deliberately not part of internal/retrieval. Retrieval answers "what is
// topically relevant to this question" and is allowed to be approximate;
// browsing answers "which messages exist, in what order, and what replies to
// what", where an approximation is a wrong answer. Nothing here embeds,
// reranks, or scores: every result is a deterministic function of the stored
// rows, the requested filters, and the snapshot the page was taken against.
//
// The package is transport independent — no MCP, HTTP, or CLI types cross this
// boundary — and its workspace scope is fixed when the Service is constructed.
// A request carries filters and a server-issued reference and nothing else, so
// there is no argument through which a caller can reach another workspace, name
// a credential, or inject a filter expression the store did not build itself.
//
// The stored model it reads is owned by ingestion-design.md §2.5.
package mailbox

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/suankan/pocket-advisor/internal/workspace"
)

// Config bounds what one request may cost. Kept here rather than in
// internal/config because these are limits of this read path, not deployment
// settings: a caller may ask for fewer, never for more.
type Config struct {
	// DefaultLimit applies when a request does not ask for a page size.
	DefaultLimit int
	// MaxLimit caps a requested page size. Exceeding it is clamped and
	// reported, not rejected: a caller asking for too much still has a
	// well-defined, resumable answer.
	MaxLimit int
	// MaxParticipants bounds the participant list on a conversation summary.
	MaxParticipants int
	// MaxCandidates bounds candidate conversations rendered by one request.
	MaxCandidates int
}

// DefaultConfig is the compiled default bound set.
func DefaultConfig() Config {
	return Config{
		DefaultLimit:    25,
		MaxLimit:        200,
		MaxParticipants: 12,
		MaxCandidates:   100,
	}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.DefaultLimit <= 0 {
		c.DefaultLimit = d.DefaultLimit
	}
	if c.MaxLimit <= 0 {
		c.MaxLimit = d.MaxLimit
	}
	if c.MaxParticipants <= 0 {
		c.MaxParticipants = d.MaxParticipants
	}
	if c.MaxCandidates <= 0 {
		c.MaxCandidates = d.MaxCandidates
	}
	return c
}

// Relationship methods: how one message was linked to its parent, decided at
// read time from the stored reply headers.
const (
	// RelationshipRoot is a message that named no parent and no ancestors.
	RelationshipRoot = "root"
	// RelationshipInReplyTo is the exact RFC 5322 linkage: one In-Reply-To
	// identifier, owned by exactly one document in this workspace.
	RelationshipInReplyTo = "in_reply_to"
	// RelationshipReferencesRecovery is the labelled fallback: the parent
	// itself is absent or unusable, so the nearest resolvable References
	// ancestor stands in. It is an ancestor claim, never an exact parent one.
	RelationshipReferencesRecovery = "references_recovery"
	// RelationshipUnresolved is a message that named a parent or ancestors,
	// none of which resolve to a single document here. The message stays in
	// the conversation; it simply has no edge.
	RelationshipUnresolved = "unresolved"
)

// Warning codes. Anything that quietly changes what a caller receives reports
// itself here rather than in a log line nobody reads.
const (
	// WarnAmbiguousParent: In-Reply-To named more than one identifier, so no
	// single parent claim can be honoured.
	WarnAmbiguousParent = "ambiguous_parent"
	// WarnDuplicateIdentifier: an identifier is claimed by more than one
	// document, so linking to it would pick a copy arbitrarily.
	WarnDuplicateIdentifier = "duplicate_identifier"
	// WarnUndatedExcluded: a date bound was applied, and messages whose Date
	// header was absent or unparsable cannot satisfy it.
	WarnUndatedExcluded = "undated_messages_excluded"
	// WarnLimitClamped: the requested page size exceeded Config.MaxLimit.
	WarnLimitClamped = "limit_clamped"
)

// Omission reasons. An omission is content the result deliberately does not
// carry; a warning is a caveat about the content it does.
const (
	// OmitCollapsedMessages: conversation collapse hid further matches.
	OmitCollapsedMessages = "collapsed_messages"
	// OmitMissingAncestor: identifiers this conversation refers to were never
	// ingested here. Tolerated by design — the conversation is still returned.
	OmitMissingAncestor = "missing_ancestor"
	// OmitParticipantsTruncated: a summary listed more participants than the
	// configured bound.
	OmitParticipantsTruncated = "participants_truncated"
)

// Warning is one caveat, attributed to a document where it belongs to one.
//
// Codes are stable strings and nothing else travels: no address, subject, or
// identifier, because a warning is the part of a result most likely to be
// logged verbatim by whatever consumes it.
type Warning struct {
	Code  string `json:"code"`
	DocID string `json:"doc_id,omitempty"`
}

// Omission is something left out, and how much of it.
type Omission struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

// ErrOwnerIdentitiesRequired means the operation needs the workspace owner's
// private mailbox configuration. The error intentionally does not disclose an
// identity or workspace name.
var ErrOwnerIdentitiesRequired = errors.New("owner identities are required for direction-dependent mailbox operations")

// ErrUnknownReference is returned for a reference this server did not issue,
// or one naming a message that is not in this workspace. The two are one error
// on purpose: distinguishing them would let a caller probe for the existence of
// documents it cannot otherwise see.
var ErrUnknownReference = errors.New("reference does not name a message in this workspace")

// Service answers browse and conversation queries for exactly one workspace.
//
// It holds no request state. The workspace is a construction argument and is
// never read from a request, which is what makes cross-workspace access
// unreachable rather than merely unimplemented (workspace-isolation.md §3).
type Service struct {
	store           Store
	workspace       string
	cfg             Config
	log             *slog.Logger
	ownerIdentities []string
}

// New wires a Service and refuses to build an unscoped one.
func New(store Store, workspaceID string, cfg Config, log *slog.Logger) (*Service, error) {
	return NewWithOwnerIdentities(store, workspaceID, nil, cfg, log)
}

// NewWithOwnerIdentities wires a service with the private, workspace-scoped
// owner mailboxes resolved from workspace configuration. They are normalized at
// this boundary so callers cannot accidentally compare display strings.
func NewWithOwnerIdentities(store Store, workspaceID string, ownerIdentities []string, cfg Config, log *slog.Logger) (*Service, error) {
	if store == nil {
		return nil, errors.New("mailbox service requires a store")
	}
	if workspaceID == "" {
		return nil, errors.New("mailbox service requires a workspace scope")
	}
	owners := make([]string, 0, len(ownerIdentities))
	seen := make(map[string]struct{}, len(ownerIdentities))
	for _, identity := range ownerIdentities {
		address, err := workspace.NormalizeMailbox(identity)
		if err != nil {
			return nil, errors.New("mailbox service owner identity is not a valid mailbox")
		}
		if _, duplicate := seen[address]; duplicate {
			return nil, errors.New("mailbox service owner identities contain a duplicate mailbox")
		}
		seen[address] = struct{}{}
		owners = append(owners, address)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{store: store, workspace: workspaceID, cfg: cfg.withDefaults(), log: log, ownerIdentities: owners}, nil
}

func (s *Service) requireOwnerIdentities() error {
	if len(s.ownerIdentities) == 0 {
		return ErrOwnerIdentitiesRequired
	}
	return nil
}

// Workspace is the fixed scope this service serves, for diagnostics.
func (s *Service) Workspace() string { return s.workspace }

// warnings accumulates warnings in first-seen order, without repeating a code
// for the same document.
type warnings struct {
	seen map[Warning]struct{}
	list []Warning
}

func newWarnings() *warnings { return &warnings{seen: map[Warning]struct{}{}} }

func (w *warnings) add(code, docID string) {
	entry := Warning{Code: code, DocID: docID}
	if _, dup := w.seen[entry]; dup {
		return
	}
	w.seen[entry] = struct{}{}
	w.list = append(w.list, entry)
}

func (w *warnings) all() []Warning { return nonNilWarnings(w.list) }

// omissions accumulates omission counts, one entry per reason, in first-seen
// order.
type omissions struct {
	index map[string]int
	list  []Omission
}

func newOmissions() *omissions { return &omissions{index: map[string]int{}} }

func (o *omissions) add(reason string, count int) {
	if count <= 0 {
		return
	}
	if i, ok := o.index[reason]; ok {
		o.list[i].Count += count
		return
	}
	o.index[reason] = len(o.list)
	o.list = append(o.list, Omission{Reason: reason, Count: count})
}

func (o *omissions) all() []Omission { return nonNilOmissions(o.list) }

// Always emit arrays rather than nulls: "nothing was omitted" and "the field is
// absent" are different claims, and a consumer should not have to tell them
// apart.
func nonNilWarnings(w []Warning) []Warning {
	if w == nil {
		return []Warning{}
	}
	return w
}

func nonNilOmissions(o []Omission) []Omission {
	if o == nil {
		return []Omission{}
	}
	return o
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func errUnsupportedOrder(o Order) error {
	return fmt.Errorf("unsupported sort order %q", o)
}
