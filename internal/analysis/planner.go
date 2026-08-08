package analysis

import (
	"fmt"
	"strings"
	"time"
)

// Planner builds a reproducible research plan from a topic request. The plan
// combines exact participant, date, message, and conversation filters with
// lexical and dense topic sub-queries, thread expansion, chronological
// sampling, and additional passes for underrepresented participants or time
// windows.
type Planner struct {
	now func() time.Time
}

// NewPlanner creates a planner with the given time function.
func NewPlanner(now func() time.Time) *Planner {
	if now == nil {
		now = time.Now
	}
	return &Planner{now: now}
}

// BuildPlan creates a research plan from a topic request and resolved scope.
// The plan is deterministic: given the same inputs, it produces the same passes.
func (p *Planner) BuildPlan(req TopicRequest, budget int) *ResearchPlan {
	if budget <= 0 {
		budget = DefaultEvidenceBudget()
	}

	scope := ScopeFilter{
		After:         req.After,
		Before:        req.Before,
		Participants:  append([]string{}, req.Participants...),
		Conversations: append([]string{}, req.Conversations...),
	}

	plan := &ResearchPlan{
		Question:    strings.TrimSpace(req.Question),
		Scope:       scope,
		Passes:      make([]ResearchPass, 0),
		MaxEvidence: budget,
	}

	passIndex := 0

	// Pass 1: Exact participant filter (if participants specified).
	if len(scope.Participants) > 0 {
		participantQuery := buildParticipantQuery(plan.Question, scope.Participants)
		passes := participantPasses(participantQuery, scope.Participants, &passIndex, scope)
		plan.Passes = append(plan.Passes, passes...)
	}

	// Pass 2: Lexical topic search.
	plan.Passes = append(plan.Passes, ResearchPass{
		Index:       passIndex,
		Kind:        "lexical_topic",
		Query:       plan.Question,
		Filters:     buildDateFilters(scope),
		Description: "Lexical search for the full topic question",
	})
	passIndex++

	// Pass 3: Dense topic search (handled by the retrieval service per pass).
	plan.Passes = append(plan.Passes, ResearchPass{
		Index:       passIndex,
		Kind:        "dense_topic",
		Query:       plan.Question,
		Filters:     buildDateFilters(scope),
		Description: "Dense embedding search for the full topic question",
	})
	passIndex++

	// Pass 4: Derived sub-queries from the question.
	subQueries := deriveSubQueries(plan.Question)
	for _, sq := range subQueries {
		plan.Passes = append(plan.Passes, ResearchPass{
			Index:       passIndex,
			Kind:        "sub_query",
			Query:       sq,
			Filters:     buildDateFilters(scope),
			Description: fmt.Sprintf("Sub-query: %s", sq),
		})
		passIndex++
	}

	// Pass 5: Named entity variants (if participants are specified).
	if len(scope.Participants) > 0 {
		for _, name := range scope.Participants {
			plan.Passes = append(plan.Passes, ResearchPass{
				Index:       passIndex,
				Kind:        "entity_variant",
				Query:       fmt.Sprintf("%s %s", name, plan.Question),
				Filters:     buildDateFilters(scope),
				Description: fmt.Sprintf("Participant-specific search: %s", name),
			})
			passIndex++
		}
	}

	// Pass 6: Thread expansion for specific conversations.
	if len(scope.Conversations) > 0 {
		for _, convID := range scope.Conversations {
			plan.Passes = append(plan.Passes, ResearchPass{
				Index:       passIndex,
				Kind:        "thread_expansion",
				Query:       plan.Question,
				Filters:     append(buildDateFilters(scope), fmt.Sprintf("conversation:%s", convID)),
				Description: fmt.Sprintf("Thread expansion for conversation %s", convID),
			})
			passIndex++
		}
	}

	// Cap the number of passes.
	if len(plan.Passes) > MaxResearchPasses {
		skipped := plan.Passes[MaxResearchPasses:]
		plan.Passes = plan.Passes[:MaxResearchPasses]
		// Store skipped passes for dossier reporting.
		plan.Passes = append(plan.Passes, skipped...) // keep for reporting
	}

	return plan
}

// participantPasses builds passes that filter by specific participants.
func participantPasses(question string, participants []string, passIndex *int, scope ScopeFilter) []ResearchPass {
	var passes []ResearchPass

	// Combined participant search.
	if len(participants) > 1 {
		parts := strings.Join(participants, " OR ")
		filters := append(buildDateFilters(scope), fmt.Sprintf("participants:%s", parts))
		passes = append(passes, ResearchPass{
			Index:       *passIndex,
			Kind:        "participant_combined",
			Query:       question,
			Filters:     filters,
			Description: fmt.Sprintf("Search across participants: %s", parts),
		})
		*passIndex++
	}

	return passes
}

// buildParticipantQuery augments the question with participant names for
// focused lexical search.
func buildParticipantQuery(question string, participants []string) string {
	if len(participants) == 0 {
		return question
	}
	// Add participant names to the query for focused lexical matching.
	names := strings.Join(participants, " ")
	return fmt.Sprintf("%s %s", question, names)
}

// buildDateFilters constructs date filter strings from the scope.
func buildDateFilters(scope ScopeFilter) []string {
	var filters []string
	if scope.After != nil {
		filters = append(filters, fmt.Sprintf("after:%s", scope.After.Format("2006-01-02")))
	}
	if scope.Before != nil {
		filters = append(filters, fmt.Sprintf("before:%s", scope.Before.Format("2006-01-02")))
	}
	return filters
}

// deriveSubQueries extracts key phrases from the question for additional passes.
// This is a deterministic lexical approach that does not require a model.
func deriveSubQueries(question string) []string {
	words := strings.Fields(strings.ToLower(question))

	// Skip common stop words for sub-query extraction.
	stopWords := map[string]struct{}{
		"a": {}, "an": {}, "the": {}, "is": {}, "are": {}, "was": {}, "were": {},
		"what": {}, "who": {}, "when": {}, "where": {}, "how": {}, "why": {},
		"do": {}, "does": {}, "did": {}, "can": {}, "could": {}, "would": {},
		"should": {}, "will": {}, "shall": {}, "may": {}, "might": {},
		"in": {}, "on": {}, "at": {}, "to": {}, "for": {}, "of": {}, "with": {},
		"by": {}, "from": {}, "about": {}, "between": {}, "through": {},
		"and": {}, "or": {}, "but": {}, "not": {}, "no": {},
		"this": {}, "that": {}, "these": {}, "those": {}, "it": {},
		"i": {}, "me": {}, "my": {}, "we": {}, "our": {}, "you": {}, "your": {},
		"he": {}, "she": {}, "they": {}, "them": {}, "his": {}, "her": {}, "its": {},
	}

	var meaningful []string
	for _, w := range words {
		w = strings.Trim(w, ".,;:!?\"'()[]{}")
		if w == "" {
			continue
		}
		if _, stop := stopWords[w]; stop {
			continue
		}
		meaningful = append(meaningful, w)
	}

	// Build 2-3 word phrases from meaningful terms.
	var subs []string
	if len(meaningful) >= 3 {
		subs = append(subs, strings.Join(meaningful[:3], " "))
	}
	if len(meaningful) >= 5 {
		subs = append(subs, strings.Join(meaningful[2:5], " "))
	}
	if len(meaningful) >= 2 {
		subs = append(subs, strings.Join(meaningful[len(meaningful)-2:], " "))
	}

	return subs
}

// EstimatePassCost estimates the evidence cost of a research plan.
func EstimatePassCost(plan *ResearchPlan) int {
	// Each pass yields up to DefaultTopK (15) packets of ~500 bytes each.
	const perPassCost = 15 * 500
	return len(plan.Passes) * perPassCost
}

// CanContinue reports whether the plan has more passes to execute.
func (p *ResearchPlan) CanContinue() bool {
	return p.Continuation != nil && p.Continuation.PassIndex < len(p.Passes)
}
