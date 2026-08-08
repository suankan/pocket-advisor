package analysis

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/suankan/pocket-advisor/internal/retrieval"
)

// Retriever is the retrieval boundary used by the analysis executor.
// retrieval.Service satisfies this interface.
type Retriever interface {
	Query(context.Context, retrieval.Request) (*retrieval.Result, error)
}

// Executor runs research plans against a retrieval service and assembles
// dossiers from the collected evidence.
type Executor struct {
	Retriever Retriever
	Log       *slog.Logger
	Planner   *Planner
	now       func() time.Time
	random    io.Reader
}

// NewExecutor creates an executor from a retriever and logger.
func NewExecutor(retr Retriever, log *slog.Logger) *Executor {
	now := time.Now
	return &Executor{
		Retriever: retr,
		Log:       log,
		Planner:   NewPlanner(now),
		now:       now,
		random:    rand.Reader,
	}
}

// ExecuteTopicAnalysis runs the full topic analysis pipeline. It builds a
// research plan, executes each pass, collects evidence, deduplicates, groups
// by participant, identifies conflicts, and returns a typed dossier.
func (e *Executor) ExecuteTopicAnalysis(ctx context.Context, req TopicRequest) (*TopicDossier, error) {
	if err := ValidateTopicRequest(&req); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	budget := req.EvidenceBudget
	if budget <= 0 {
		budget = DefaultEvidenceBudget()
	}

	plan := e.Planner.BuildPlan(req, budget)
	plan.MaxEvidence = budget

	dossier := &TopicDossier{
		Request:                req,
		Scope:                  plan.Scope,
		Participants:           make([]ParticipantObservation, 0),
		Conversations:          make([]ConversationSummary, 0),
		Timeline:               make([]TimelineEvent, 0),
		StatementsByParticipant: make(map[string][]ParticipantStatement),
		MostRecentByParticipant: make(map[string]*ParticipantStatement),
		ConflictingStatements:  make([]ConflictEntry, 0),
		Warnings:               make([]Warning, 0),
		EvidenceRefs:           make([]EvidenceReference, 0),
		Budget: EvidenceBudgetInfo{
			Allowed: budget,
			Unit:    UTF8ByteUnit,
		},
	}

	budgetUsed := 0
	passesPerformed := make([]ResearchPass, 0)
	passesSkipped := make([]ResearchPass, 0)
	seenChunks := make(map[string]struct{})
	seenDocIDs := make(map[string]struct{})
	resultID := e.generateResultID()

	// Execute research passes.
	for _, pass := range plan.Passes {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		// Check if budget is exhausted.
		if budgetUsed >= budget {
			passesSkipped = append(passesSkipped, pass)
			dossier.Warnings = append(dossier.Warnings, Warning{
				Kind:    WarnBudgetExhausted,
				Message: fmt.Sprintf("Research pass %d (%s) skipped: evidence budget exhausted", pass.Index, pass.Kind),
			})
			continue
		}

		result, err := e.executePass(ctx, pass, plan.Scope)
		if err != nil {
			e.Log.Warn("research pass failed", "pass", pass.Index, "kind", pass.Kind, "error", err)
			passesSkipped = append(passesSkipped, pass)
			continue
		}

		passesPerformed = append(passesPerformed, pass)

		// Collect evidence from this pass.
		passBudget := 0
		for _, packet := range result.Packets {
			// Deduplicate by chunk ID.
			if _, dup := seenChunks[packet.Match.ChunkID]; dup {
				continue
			}
			seenChunks[packet.Match.ChunkID] = struct{}{}

			// Deduplicate by document ID (keep first per document).
			docKey := packet.Document.DocID
			if _, dup := seenDocIDs[docKey]; dup {
				continue
			}
			seenDocIDs[docKey] = struct{}{}

			ref := EvidenceReference{
				ResultID:   resultID,
				Rank:       len(dossier.EvidenceRefs) + 1,
				FullRef:    fmt.Sprintf("%s:E%d", resultID, len(dossier.EvidenceRefs)+1),
				DocumentID: packet.Document.DocID,
			}
			dossier.EvidenceRefs = append(dossier.EvidenceRefs, ref)

			// Track participant.
			if packet.Document.From != "" {
				e.trackParticipant(dossier, packet, ref)
			}

			// Track conversation.
			if packet.Document.ThreadID != "" {
				e.trackConversation(dossier, packet, ref)
			}

			// Track timeline event.
			if packet.Document.Date != nil {
				e.trackTimelineEvent(dossier, packet, ref)
			}

			// Budget accounting.
			textLen := utf8.RuneCountInString(packet.Text)
			passBudget += textLen
			budgetUsed += textLen
		}
	}

	// Assemble the dossier.
	dossier.PassesPerformed = passesPerformed
	dossier.PassesSkipped = passesSkipped
	dossier.Budget.Used = budgetUsed
	dossier.Budget.PassesRun = len(passesPerformed)

	// Detect conflicts.
	e.detectConflicts(dossier)

	// Sort timeline.
	SortTimelineChronological(dossier.Timeline)

	// Cap timeline.
	if len(dossier.Timeline) > MaxTimelineEvents {
		dossier.Timeline = dossier.Timeline[:MaxTimelineEvents]
		dossier.Warnings = append(dossier.Warnings, Warning{
			Kind:    WarnPartialCoverage,
			Message: fmt.Sprintf("Timeline truncated to %d events", MaxTimelineEvents),
		})
	}

	// Cap participants.
	if len(dossier.Participants) > MaxParticipantsShown {
		dossier.Participants = dossier.Participants[:MaxParticipantsShown]
		dossier.Warnings = append(dossier.Warnings, Warning{
			Kind:    WarnPartialCoverage,
			Message: fmt.Sprintf("Participant list truncated to %d", MaxParticipantsShown),
		})
	}

	// Coverage warnings.
	e.addCoverageWarnings(dossier)

	dossier.Complete = true
	return dossier, nil
}

// ExecuteReview runs the outstanding-item review pipeline.
func (e *Executor) ExecuteReview(ctx context.Context, req ReviewRequest, outboxItems []CandidateClassification) (*ReviewDossier, error) {
	if err := ValidateReviewRequest(&req); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	maxItems := req.MaxItems
	if maxItems <= 0 {
		maxItems = DefaultMaxItems()
	}

	dossier := &ReviewDossier{
		Request:         req,
		Classifications: make([]CandidateClassification, 0),
		TotalCandidates: len(outboxItems),
		Warnings:        make([]Warning, 0),
		Budget: EvidenceBudgetInfo{
			Allowed: DefaultEvidenceBudget(),
			Unit:    UTF8ByteUnit,
		},
	}

	// Filter by requested IDs if specified.
	filtered := outboxItems
	if len(req.CandidateIDs) > 0 {
		idSet := make(map[string]struct{}, len(req.CandidateIDs))
		for _, id := range req.CandidateIDs {
			idSet[id] = struct{}{}
		}
		filtered = make([]CandidateClassification, 0)
		for _, item := range outboxItems {
			if _, ok := idSet[item.ConversationID]; ok {
				filtered = append(filtered, item)
			}
		}
	}

	// Apply max items bound.
	for i := 0; i < len(filtered) && i < maxItems; i++ {
		dossier.Classifications = append(dossier.Classifications, filtered[i])
	}

	dossier.ReturnedCount = len(dossier.Classifications)
	dossier.Budget.Used = len(dossier.Classifications) * 500 // rough estimate
	dossier.Budget.PassesRun = 1
	dossier.Complete = true

	return dossier, nil
}

// executePass runs a single research pass against the retrieval service.
func (e *Executor) executePass(ctx context.Context, pass ResearchPass, scope ScopeFilter) (*retrieval.Result, error) {
	// Combine the pass query with scope filters.
	query := pass.Query

	// Add date context to the query for retrieval.
	if scope.After != nil || scope.Before != nil {
		dateHint := buildDateHint(scope)
		if dateHint != "" {
			query = query + " " + dateHint
		}
	}

	req := retrieval.Request{
		Question: query,
		TopK:     15,
	}

	result, err := e.Retriever.Query(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("pass %d (%s): %w", pass.Index, pass.Kind, err)
	}
	return result, nil
}

// buildDateHint constructs a natural-language date hint for the query.
func buildDateHint(scope ScopeFilter) string {
	var parts []string
	if scope.After != nil {
		parts = append(parts, fmt.Sprintf("after %s", scope.After.Format("January 2006")))
	}
	if scope.Before != nil {
		parts = append(parts, fmt.Sprintf("before %s", scope.Before.Format("January 2006")))
	}
	return strings.Join(parts, " and ")
}

// trackParticipant records a participant observation from a packet.
func (e *Executor) trackParticipant(dossier *TopicDossier, packet retrieval.Packet, ref EvidenceReference) {
	from := packet.Document.From
	date := ""
	if packet.Document.Date != nil {
		date = packet.Document.Date.Format(time.RFC3339)
	}

	// Find or create participant observation.
	var obs *ParticipantObservation
	for i := range dossier.Participants {
		if dossier.Participants[i].Address == from {
			obs = &dossier.Participants[i]
			break
		}
	}
	if obs == nil {
		dossier.Participants = append(dossier.Participants, ParticipantObservation{
			Address:    from,
			Statements: make([]ParticipantStatement, 0),
		})
		obs = &dossier.Participants[len(dossier.Participants)-1]
	}

	obs.MessageCount++
	if date != "" {
		if obs.EarliestDate == "" || date < obs.EarliestDate {
			obs.EarliestDate = date
		}
		if obs.LatestDate == "" || date > obs.LatestDate {
			obs.LatestDate = date
		}
	}

	// Create statement from snippet.
	snippet := packet.Match.Snippet
	position := snippet
	if len(position) > 200 {
		position = position[:200] + "..."
	}

	statement := ParticipantStatement{
		Reference: ref,
		Date:      date,
		Summary:   snippet,
		Position:  position,
		Relevance: packet.Match.Score,
	}
	obs.Statements = append(obs.Statements, statement)

	// Track in dossier map.
	dossier.StatementsByParticipant[from] = append(dossier.StatementsByParticipant[from], statement)

	// Update most recent.
	current, ok := dossier.MostRecentByParticipant[from]
	if !ok || date > current.Date {
		stmt := statement
		dossier.MostRecentByParticipant[from] = &stmt
		obs.MostRecentStatement = &stmt
	}
}

// trackConversation records a conversation observation from a packet.
func (e *Executor) trackConversation(dossier *TopicDossier, packet retrieval.Packet, ref EvidenceReference) {
	threadID := ""
	threadID = packet.Document.ThreadID
	if threadID == "" {
		return
	}

	// Find or create conversation summary.
	var conv *ConversationSummary
	for i := range dossier.Conversations {
		if dossier.Conversations[i].ConversationID == threadID {
			conv = &dossier.Conversations[i]
			break
		}
	}
	if conv == nil {
		dossier.Conversations = append(dossier.Conversations, ConversationSummary{
			ConversationID: threadID,
			Participants:   make([]string, 0),
			References:     make([]EvidenceReference, 0),
		})
		conv = &dossier.Conversations[len(dossier.Conversations)-1]
	}

	conv.Subject = packet.Document.Title
	conv.MessageCount++
	conv.References = append(conv.References, ref)

	date := ""
	if packet.Document.Date != nil {
		date = packet.Document.Date.Format(time.RFC3339)
	}
	if date != "" {
		if conv.EarliestDate == "" || date < conv.EarliestDate {
			conv.EarliestDate = date
		}
		if conv.LatestDate == "" || date > conv.LatestDate {
			conv.LatestDate = date
		}
	}

	// Track participants.
	if packet.Document.From != "" {
		found := false
		for _, p := range conv.Participants {
			if p == packet.Document.From {
				found = true
				break
			}
		}
		if !found {
			conv.Participants = append(conv.Participants, packet.Document.From)
		}
	}
}

// trackTimelineEvent records a timeline event from a packet.
func (e *Executor) trackTimelineEvent(dossier *TopicDossier, packet retrieval.Packet, ref EvidenceReference) {
	if packet.Document.Date == nil {
		return
	}

	event := TimelineEvent{
		Date:        packet.Document.Date.Format(time.RFC3339),
		Reference:   ref,
		Summary:     packet.Match.Snippet,
		Participant: packet.Document.From,
	}
	dossier.Timeline = append(dossier.Timeline, event)
}

// detectConflicts identifies participants with materially different statements.
func (e *Executor) detectConflicts(dossier *TopicDossier) {
	for participant, statements := range dossier.StatementsByParticipant {
		if len(statements) < 2 {
			continue
		}

		// Simple conflict detection: if a participant has statements with
		// significantly different content, flag them as conflicting.
		for i := 0; i < len(statements); i++ {
			for j := i + 1; j < len(statements); j++ {
				if statementsDiffer(statements[i], statements[j]) {
					dossier.ConflictingStatements = append(dossier.ConflictingStatements, ConflictEntry{
						Participant: participant,
						Statements:  []ParticipantStatement{statements[i], statements[j]},
						Topic:       dossier.Request.Question,
					})

					// Add conflict references to the participant.
					for k := range dossier.Participants {
						if dossier.Participants[k].Address == participant {
							dossier.Participants[k].ConflictsWith = append(
								dossier.Participants[k].ConflictsWith,
								statements[j].Reference,
							)
							break
						}
					}

					// Add warning.
					dossier.Warnings = append(dossier.Warnings, Warning{
						Kind:    WarnConflictingEvidence,
						Message: fmt.Sprintf("Participant %s has conflicting statements at %s and %s", participant, statements[i].Reference.FullRef, statements[j].Reference.FullRef),
					})
					break
				}
			}
		}
	}
}

// statementsDiffer checks if two statements are materially different.
// This uses simple lexical comparison of the position text.
func statementsDiffer(a, b ParticipantStatement) bool {
	if a.Position == "" || b.Position == "" {
		return false
	}
	// Normalize and compare.
	normA := strings.ToLower(strings.TrimSpace(a.Position))
	normB := strings.ToLower(strings.TrimSpace(b.Position))
	if normA == normB {
		return false
	}
	// Check if the statements share significant vocabulary overlap.
	wordsA := make(map[string]struct{})
	for _, w := range strings.Fields(normA) {
		if len(w) > 3 {
			wordsA[w] = struct{}{}
		}
	}
	overlap := 0
	for _, w := range strings.Fields(normB) {
		if len(w) > 3 {
			if _, ok := wordsA[w]; ok {
				overlap++
			}
		}
	}
	// If less than 30% overlap, consider them materially different.
	totalWords := len(wordsA) + len(strings.Fields(normB))
	if totalWords == 0 {
		return false
	}
	return float64(overlap)/float64(totalWords) < 0.3
}

// addCoverageWarnings adds warnings about incomplete coverage.
func (e *Executor) addCoverageWarnings(dossier *TopicDossier) {
	// Check participant coverage.
	if len(dossier.Scope.Participants) > 0 {
		requested := make(map[string]struct{}, len(dossier.Scope.Participants))
		for _, p := range dossier.Scope.Participants {
			requested[strings.ToLower(p)] = struct{}{}
		}
		found := 0
		for _, obs := range dossier.Participants {
			if _, ok := requested[strings.ToLower(obs.Address)]; ok {
				found++
			}
		}
		if found < len(dossier.Scope.Participants) {
			dossier.Warnings = append(dossier.Warnings, Warning{
				Kind:    WarnParticipantCoverage,
				Message: fmt.Sprintf("Only %d of %d requested participants found in evidence", found, len(dossier.Scope.Participants)),
			})
		}
	}

	// Check time coverage.
	if dossier.Scope.After != nil || dossier.Scope.Before != nil {
		if len(dossier.Timeline) == 0 {
			dossier.Warnings = append(dossier.Warnings, Warning{
				Kind:    WarnTimeCoverage,
				Message: "No events found in the specified time range",
			})
		}
	}

	// Check if no evidence was found at all.
	if len(dossier.EvidenceRefs) == 0 {
		dossier.Warnings = append(dossier.Warnings, Warning{
			Kind:    WarnNoSupport,
			Message: "No supporting evidence found for the research question",
		})
	}
}

// generateResultID creates a collision-free result namespace identifier.
func (e *Executor) generateResultID() string {
	raw := make([]byte, 6)
	if _, err := io.ReadFull(e.random, raw); err != nil {
		// Fallback: use timestamp.
		return fmt.Sprintf("A%012d", e.now().UnixNano()%1000000000000)
	}
	return "A" + hex.EncodeToString(raw)
}
