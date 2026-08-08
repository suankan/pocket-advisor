// Package analysis provides transport-independent research planning and
// dossier assembly for broad corpus queries. It builds multi-pass research
// plans that combine lexical and dense search, thread expansion,
// chronological sampling, and coverage balancing, then assembles typed
// dossiers containing cited evidence grouped by participant with
// deterministic continuation cursors.
//
// Analysis does not generate answers. It produces structured evidence
// dossiers that an MCP client or generation service consumes with explicit
// citations.
package analysis

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Maximum bounds for analysis requests.
const (
	MaxTopicRunes        = 8192
	MaxParticipants      = 50
	MaxConversations     = 100
	MaxEvidenceBudget    = 500000 // UTF-8 bytes aggregate across all passes
	MaxResearchPasses    = 20
	MaxResultPageBytes   = 48 * 1024
	AbsoluteResponseBytes = 51200
	MaxParticipantsShown = 30
	MaxTimelineEvents    = 100
	MaxStatementsPerParticipant = 20
	UTF8ByteUnit         = "utf8_bytes"
)

// TopicRequest is the input to analyze_topic.
type TopicRequest struct {
	// Question is the research topic or question.
	Question string `json:"question"`
	// Participants is an optional list of mailbox addresses or display names.
	Participants []string `json:"participants,omitempty"`
	// After restricts results to messages on or after this RFC 3339 date.
	After *time.Time `json:"after,omitempty"`
	// Before restricts results to messages on or before this RFC 3339 date.
	Before *time.Time `json:"before,omitempty"`
	// Conversations is an optional list of conversation or thread IDs.
	Conversations []string `json:"conversations,omitempty"`
	// EvidenceBudget is the total UTF-8 byte budget across all passes.
	// Zero means the server default.
	EvidenceBudget int `json:"evidence_budget,omitempty"`
	// Cursor is an opaque continuation cursor from a previous call.
	Cursor string `json:"cursor,omitempty"`
}

// OutboxItem represents one email conversation awaiting a reply.
type OutboxItem struct {
	ConversationID string `json:"conversation_id"`
	Subject        string `json:"subject"`
	// LatestInbound is the most recent inbound message in the conversation.
	LatestInbound EvidenceReference `json:"latest_inbound"`
	// Messages is the bounded conversation in chronological order.
	Messages []ConversationMessage `json:"messages"`
	// ParticipantCount is the number of distinct participants.
	ParticipantCount int `json:"participant_count"`
	// LastActivity is the date of the most recent message.
	LastActivity string `json:"last_activity"`
}

// ConversationMessage is one message in a conversation.
type ConversationMessage struct {
	Reference    string  `json:"reference"`
	From         string  `json:"from"`
	To           string  `json:"to"`
	Date         string  `json:"date"`
	Subject      string  `json:"subject"`
	Snippet      string  `json:"snippet"`
	IsInbound    bool    `json:"is_inbound"`
	HasQuestion  bool    `json:"has_question"`
	TextAvailable bool   `json:"text_available"`
}

// EvidenceReference is a collision-free citation into the result namespace.
type EvidenceReference struct {
	ResultID   string `json:"result_id"`
	Rank       int    `json:"rank"`
	FullRef    string `json:"full_ref"`
	DocumentID string `json:"document_id"`
}

// ResearchPass describes one search pass within a research plan.
type ResearchPass struct {
	Index       int      `json:"index"`
	Kind        string   `json:"kind"`
	Query       string   `json:"query"`
	Filters     []string `json:"filters"`
	Description string   `json:"description"`
}

// ResearchPlan is the full plan for a topic analysis.
type ResearchPlan struct {
	Question     string         `json:"question"`
	Scope        ScopeFilter    `json:"scope"`
	Passes       []ResearchPass `json:"passes"`
	MaxEvidence  int            `json:"max_evidence"`
	Continuation *ContinuationState `json:"continuation,omitempty"`
}

// ScopeFilter captures the resolved date range, participants, and conversations.
type ScopeFilter struct {
	After        *time.Time `json:"after,omitempty"`
	Before       *time.Time `json:"before,omitempty"`
	Participants []string   `json:"participants,omitempty"`
	Conversations []string  `json:"conversations,omitempty"`
}

// ParticipantObservation tracks one participant's evidence across the analysis.
type ParticipantObservation struct {
	Address        string                `json:"address"`
	MessageCount   int                   `json:"message_count"`
	EarliestDate   string                `json:"earliest_date,omitempty"`
	LatestDate     string                `json:"latest_date,omitempty"`
	Statements     []ParticipantStatement `json:"statements"`
	MostRecentStatement *ParticipantStatement `json:"most_recent_statement,omitempty"`
	ConflictsWith  []EvidenceReference   `json:"conflicts_with,omitempty"`
}

// ParticipantStatement is one source statement by a participant.
type ParticipantStatement struct {
	Reference     EvidenceReference `json:"reference"`
	Date          string            `json:"date"`
	Summary       string            `json:"summary"`
	Position      string            `json:"position"`
	Relevance     float64           `json:"relevance"`
}

// TimelineEvent is one event in the chronological timeline.
type TimelineEvent struct {
	Date        string            `json:"date"`
	Reference   EvidenceReference `json:"reference"`
	Summary     string            `json:"summary"`
	Participant string            `json:"participant"`
}

// Warning represents a coverage or quality warning.
type Warning struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// Warning kinds.
const (
	WarnParticipantCoverage = "participant_coverage"
	WarnTimeCoverage        = "time_coverage"
	WarnNoSupport           = "no_support_found"
	WarnPartialCoverage     = "partial_coverage"
	WarnThreadExpansion     = "thread_expansion_skipped"
	WarnBudgetExhausted     = "budget_exhausted"
	WarnDeduplication       = "deduplication_applied"
	WarnAmbiguousPosition   = "ambiguous_position"
	WarnConflictingEvidence = "conflicting_evidence"
	WarnConversationSkipped = "conversation_skipped"
	WarnMissingParent       = "missing_parent"
	WarnOutsideDateRange    = "outside_date_range"
)

// Classification for outstanding-item review.
const (
	ClassificationActionRequired      = "likely_action_required"
	ClassificationNoActionRequired    = "likely_no_action_required"
	ClassificationUncertain           = "uncertain"
)

// TopicDossier is the complete output of analyze_topic.
type TopicDossier struct {
	// Original request and effective scope.
	Request       TopicRequest `json:"request"`
	Scope         ScopeFilter  `json:"scope"`

	// Research passes performed and skipped.
	PassesPerformed []ResearchPass `json:"passes_performed"`
	PassesSkipped   []ResearchPass `json:"passes_skipped,omitempty"`

	// Participants observed in retrieved evidence.
	Participants []ParticipantObservation `json:"participants"`

	// Conversations observed.
	Conversations []ConversationSummary `json:"conversations"`

	// Chronological event timeline.
	Timeline []TimelineEvent `json:"timeline"`

	// Source statements grouped by participant.
	StatementsByParticipant map[string][]ParticipantStatement `json:"statements_by_participant"`

	// Most recent supported statement per participant.
	MostRecentByParticipant map[string]*ParticipantStatement `json:"most_recent_by_participant"`

	// Earlier conflicting or materially different statements.
	ConflictingStatements []ConflictEntry `json:"conflicting_statements,omitempty"`

	// Coverage warnings.
	Warnings []Warning `json:"warnings"`

	// Budget information.
	Budget EvidenceBudgetInfo `json:"budget"`

	// Collision-free evidence references.
	EvidenceRefs []EvidenceReference `json:"evidence_refs"`

	// Continuation cursor.
	Complete   bool   `json:"complete"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// ConversationSummary summarizes one conversation observed in the analysis.
type ConversationSummary struct {
	ConversationID string              `json:"conversation_id"`
	Subject        string              `json:"subject"`
	MessageCount   int                 `json:"message_count"`
	Participants   []string            `json:"participants"`
	EarliestDate   string              `json:"earliest_date"`
	LatestDate     string              `json:"latest_date"`
	References     []EvidenceReference `json:"references"`
}

// ConflictEntry represents one set of conflicting statements.
type ConflictEntry struct {
	Participant    string              `json:"participant"`
	Statements     []ParticipantStatement `json:"statements"`
	Topic          string              `json:"topic"`
}

// EvidenceBudgetInfo describes budget consumption.
type EvidenceBudgetInfo struct {
	Used      int    `json:"used"`
	Allowed   int    `json:"allowed"`
	Unit      string `json:"unit"`
	PassesRun int    `json:"passes_run"`
}

// ReviewRequest is the input to review_awaiting_reply.
type ReviewRequest struct {
	// CandidateIDs is an optional list of specific conversation IDs to review.
	// Empty means review all awaiting-reply candidates.
	CandidateIDs []string `json:"candidate_ids,omitempty"`
	// MaxItems bounds the number of candidates returned.
	MaxItems int `json:"max_items,omitempty"`
	// Cursor is an opaque continuation cursor from a previous call.
	Cursor string `json:"cursor,omitempty"`
}

// CandidateClassification is the agent-facing classification of one candidate.
type CandidateClassification struct {
	ConversationID   string            `json:"conversation_id"`
	Subject          string            `json:"subject"`
	Classification   string            `json:"classification"`
	EvidenceRefs     []EvidenceReference `json:"evidence_refs"`
	Reasoning        string            `json:"reasoning"`
	LatestInbound    ConversationMessage `json:"latest_inbound"`
	Messages         []ConversationMessage `json:"messages"`
	ParticipantCount int               `json:"participant_count"`
	LastActivity     string            `json:"last_activity"`
	Warnings         []Warning         `json:"warnings,omitempty"`
}

// ReviewDossier is the complete output of review_awaiting_reply.
type ReviewDossier struct {
	Request       ReviewRequest               `json:"request"`
	Classifications []CandidateClassification `json:"classifications"`
	TotalCandidates int                       `json:"total_candidates"`
	ReturnedCount  int                        `json:"returned_count"`
	Budget         EvidenceBudgetInfo          `json:"budget"`
	Warnings       []Warning                  `json:"warnings"`
	Complete       bool                       `json:"complete"`
	NextCursor     string                     `json:"next_cursor,omitempty"`
}

// ContinuationState preserves research-plan state across continuation calls.
type ContinuationState struct {
	// PassIndex is the next research pass to execute.
	PassIndex int `json:"pass_index"`
	// EvidenceOffset is the byte offset within accumulated evidence.
	EvidenceOffset int `json:"evidence_offset"`
	// SnapshotID is the result namespace for collision-free references.
	SnapshotID string `json:"snapshot_id"`
}

// DefaultEvidenceBudget returns the default evidence budget for topic analysis.
func DefaultEvidenceBudget() int {
	return MaxEvidenceBudget
}

// DefaultMaxItems returns the default max items for review.
func DefaultMaxItems() int {
	return 15
}

// ValidateTopicRequest checks input bounds.
func ValidateTopicRequest(req *TopicRequest) error {
	if strings.TrimSpace(req.Question) == "" {
		return fmt.Errorf("question is required")
	}
	if len([]rune(req.Question)) > MaxTopicRunes {
		return fmt.Errorf("question must not exceed %d Unicode characters", MaxTopicRunes)
	}
	if len(req.Participants) > MaxParticipants {
		return fmt.Errorf("participants list must not exceed %d entries", MaxParticipants)
	}
	if len(req.Conversations) > MaxConversations {
		return fmt.Errorf("conversations list must not exceed %d entries", MaxConversations)
	}
	if req.After != nil && req.Before != nil && req.After.After(*req.Before) {
		return fmt.Errorf("after date must not be after before date")
	}
	if req.EvidenceBudget < 0 {
		return fmt.Errorf("evidence_budget must be non-negative")
	}
	return nil
}

// ValidateReviewRequest checks input bounds.
func ValidateReviewRequest(req *ReviewRequest) error {
	if len(req.CandidateIDs) > MaxConversations {
		return fmt.Errorf("candidate_ids must not exceed %d entries", MaxConversations)
	}
	if req.MaxItems < 0 {
		return fmt.Errorf("max_items must be non-negative")
	}
	return nil
}

// SortParticipantsByLatest sorts participant observations by most recent date.
func SortParticipantsByLatest(parts []ParticipantObservation) {
	sort.Slice(parts, func(i, j int) bool {
		di := parts[i].LatestDate
		dj := parts[j].LatestDate
		if di == "" && dj == "" {
			return parts[i].Address < parts[j].Address
		}
		if di == "" {
			return false
		}
		if dj == "" {
			return true
		}
		return di > dj
	})
}

// SortTimelineChronological sorts timeline events by date ascending.
func SortTimelineChronological(events []TimelineEvent) {
	sort.Slice(events, func(i, j int) bool {
		return events[i].Date < events[j].Date
	})
}
