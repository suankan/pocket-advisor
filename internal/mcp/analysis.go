package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/suankan/pocket-advisor/internal/analysis"
	"github.com/suankan/pocket-advisor/internal/retrieval"
)

// AnalysisTool extends the QueryTool with analysis capabilities. It provides
// transport-independent MCP tools for topic research and outstanding-item
// review.
type AnalysisTool struct {
	// Embed the base tool for snapshot and cursor management.
	*QueryTool

	// Executor runs research plans against the retrieval service.
	Executor *analysis.Executor

	// OutboxProvider supplies awaiting-reply candidates. It is called
	// lazily when review_awaiting_reply is invoked.
	OutboxProvider func(ctx context.Context) ([]analysis.CandidateClassification, error)
}

// AnalysisToolName returns the name of the topic analysis tool.
func (t *AnalysisTool) AnalysisToolName() string {
	return "analyze_topic_" + t.normalizedWorkspace()
}

// ReviewToolName returns the name of the review awaiting-reply tool.
func (t *AnalysisTool) ReviewToolName() string {
	return "review_awaiting_reply_" + t.normalizedWorkspace()
}

// DescribeAnalysis returns the topic analysis tool definition.
func (t *AnalysisTool) DescribeAnalysis() ToolDefinition {
	title := t.Title
	if title == "" {
		title = t.Workspace
	}
	contents := ""
	if len(t.Corpus) > 0 {
		contents = "\n\nThis corpus contains:\n  - " + strings.Join(t.Corpus, "\n  - ") + "\n"
	}

	return ToolDefinition{
		Name:  t.AnalysisToolName(),
		Title: "Analyze " + title + " topic",
		Description: "Research a topic across date range, participants, and conversations in " +
			title + contents + "\n" +
			"This tool returns an evidence dossier, not an answer. Cite the complete " +
			"references shown, such as [R0123456789ab:E1]. Distinguish source statements " +
			"from inference. Disclose incomplete coverage. Return no supported conclusion " +
			"when evidence is insufficient.\n\n" +
			"When complete=false, call review_awaiting_reply with exactly next_cursor and " +
			"continue until complete=true before claiming complete coverage. Never invent " +
			"a cursor, byte range, document identifier, or workspace.\n\n" +
			"If evidence_refs is empty and complete=true, say that the corpus supplied no " +
			"supporting evidence rather than answering from general knowledge.",
		InputSchema:  analyzeInputSchema(),
		OutputSchema: dossierOutputSchema(),
		Annotations:  readOnlyAnnotations(),
	}
}

// DescribeReview returns the awaiting-reply review tool definition.
func (t *AnalysisTool) DescribeReview() ToolDefinition {
	title := t.Title
	if title == "" {
		title = t.Workspace
	}

	return ToolDefinition{
		Name:  t.ReviewToolName(),
		Title: "Review " + title + " awaiting reply",
		Description: "Review outstanding email items requiring attention in " + title +
			". Start from deterministic reply candidates. For each candidate, return " +
			"the complete bounded conversation, latest inbound request, and classify " +
			"as likely_action_required, likely_no_action_required, or uncertain.\n\n" +
			"Every classification must cite evidence and state the inference. The " +
			"server does not hard-code semantic action detection as a database fact, " +
			"and generated classifications are not written back into the corpus.\n\n" +
			"When complete=false, call review_awaiting_reply with exactly next_cursor " +
			"and continue until complete=true.",
		InputSchema:  reviewInputSchema(),
		OutputSchema: reviewDossierOutputSchema(),
		Annotations:  readOnlyAnnotations(),
	}
}

// DescribeAllAnalysis returns all tool definitions including analysis tools.
func (t *AnalysisTool) DescribeAllAnalysis() []ToolDefinition {
	defs := t.DescribeAll()
	defs = append(defs, t.DescribeAnalysis(), t.DescribeReview())
	return defs
}

// CallAnalysis dispatches analysis tool calls. It handles the two new tools
// and delegates search/read calls to the embedded QueryTool.
func (t *AnalysisTool) CallAnalysis(ctx context.Context, raw json.RawMessage) (CallToolResult, error) {
	var params rawCallParams
	if err := decodeStrict(raw, &params); err != nil {
		return CallToolResult{}, &argumentError{message: "tools/call params must be a valid object"}
	}
	if params.Name == "" {
		return CallToolResult{}, &argumentError{message: "tool name is required"}
	}

	// Dispatch to the appropriate handler.
	switch params.Name {
	case t.AnalysisToolName():
		return t.callAnalyzeTopic(ctx, params.Arguments)
	case t.ReviewToolName():
		return t.callReviewAwaitingReply(ctx, params.Arguments)
	default:
		// Delegate to the base tool for search/read calls.
		return t.QueryTool.Call(ctx, raw)
	}
}

// callAnalyzeTopic handles the analyze_topic tool call.
func (t *AnalysisTool) callAnalyzeTopic(ctx context.Context, raw json.RawMessage) (CallToolResult, error) {
	var args struct {
		Question       string   `json:"question"`
		Participants   []string `json:"participants,omitempty"`
		After          *string  `json:"after,omitempty"`
		Before         *string  `json:"before,omitempty"`
		Conversations  []string `json:"conversations,omitempty"`
		EvidenceBudget int      `json:"evidence_budget,omitempty"`
		Cursor         string   `json:"cursor,omitempty"`
	}
	if err := decodeStrict(raw, &args); err != nil {
		return CallToolResult{}, &argumentError{message: "arguments must match the advertised input schema"}
	}
	if strings.TrimSpace(args.Question) == "" {
		return CallToolResult{}, &argumentError{message: "question is required"}
	}

	req := analysis.TopicRequest{
		Question:       args.Question,
		Participants:   args.Participants,
		Conversations:  args.Conversations,
		EvidenceBudget: args.EvidenceBudget,
		Cursor:         args.Cursor,
	}

	// Parse date strings.
	if args.After != nil {
		t, err := parseFlexibleDate(*args.After)
		if err != nil {
			return CallToolResult{}, &argumentError{message: fmt.Sprintf("invalid after date: %v", err)}
		}
		req.After = &t
	}
	if args.Before != nil {
		t, err := parseFlexibleDate(*args.Before)
		if err != nil {
			return CallToolResult{}, &argumentError{message: fmt.Sprintf("invalid before date: %v", err)}
		}
		req.Before = &t
	}

	if t.Executor == nil {
		return CallToolResult{}, fmt.Errorf("analysis executor is unavailable")
	}

	dossier, err := t.Executor.ExecuteTopicAnalysis(ctx, req)
	if err != nil {
		return errorResult(err), nil
	}

	return t.buildDossierResult(dossier)
}

// callReviewAwaitingReply handles the review_awaiting_reply tool call.
func (t *AnalysisTool) callReviewAwaitingReply(ctx context.Context, raw json.RawMessage) (CallToolResult, error) {
	var args struct {
		CandidateIDs []string `json:"candidate_ids,omitempty"`
		MaxItems     int      `json:"max_items,omitempty"`
		Cursor       string   `json:"cursor,omitempty"`
	}
	if err := decodeStrict(raw, &args); err != nil {
		return CallToolResult{}, &argumentError{message: "arguments must match the advertised input schema"}
	}

	req := analysis.ReviewRequest{
		CandidateIDs: args.CandidateIDs,
		MaxItems:     args.MaxItems,
		Cursor:       args.Cursor,
	}

	if t.Executor == nil {
		return CallToolResult{}, fmt.Errorf("analysis executor is unavailable")
	}

	// Get outbox items from the provider.
	var items []analysis.CandidateClassification
	if t.OutboxProvider != nil {
		var err error
		items, err = t.OutboxProvider(ctx)
		if err != nil {
			return errorResult(fmt.Errorf("retrieve outbox: %w", err)), nil
		}
	}

	dossier, err := t.Executor.ExecuteReview(ctx, req, items)
	if err != nil {
		return errorResult(err), nil
	}

	return t.buildReviewDossierResult(dossier)
}

// buildDossierResult converts a TopicDossier into a bounded MCP CallToolResult.
func (t *AnalysisTool) buildDossierResult(dossier *analysis.TopicDossier) (CallToolResult, error) {
	text := renderDossier(dossier)
	result := CallToolResult{
		Content:           []TextContent{{Type: "text", Text: text}},
		StructuredContent: dossier,
		IsError:           false,
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return CallToolResult{}, fmt.Errorf("encode dossier: %w", err)
	}
	if len(encoded) > targetToolResultBytes {
		return CallToolResult{}, &resultSizeError{limit: targetToolResultBytes}
	}
	return result, nil
}

// buildReviewDossierResult converts a ReviewDossier into a bounded MCP CallToolResult.
func (t *AnalysisTool) buildReviewDossierResult(dossier *analysis.ReviewDossier) (CallToolResult, error) {
	text := renderReviewDossier(dossier)
	result := CallToolResult{
		Content:           []TextContent{{Type: "text", Text: text}},
		StructuredContent: dossier,
		IsError:           false,
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return CallToolResult{}, fmt.Errorf("encode review dossier: %w", err)
	}
	if len(encoded) > targetToolResultBytes {
		return CallToolResult{}, &resultSizeError{limit: targetToolResultBytes}
	}
	return result, nil
}

// renderDossier produces the readable text representation of a TopicDossier.
func renderDossier(dossier *analysis.TopicDossier) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Topic dossier for: %s\n", dossier.Request.Question)
	fmt.Fprintf(&builder, "Evidence budget: %d of %d %s\n", dossier.Budget.Used, dossier.Budget.Allowed, dossier.Budget.Unit)
	fmt.Fprintf(&builder, "Research passes: %d performed", dossier.Budget.PassesRun)
	if len(dossier.PassesSkipped) > 0 {
		fmt.Fprintf(&builder, ", %d skipped", len(dossier.PassesSkipped))
	}
	fmt.Fprintln(&builder)

	// Scope.
	if len(dossier.Scope.Participants) > 0 {
		fmt.Fprintf(&builder, "Participants filter: %s\n", strings.Join(dossier.Scope.Participants, ", "))
	}
	if dossier.Scope.After != nil {
		fmt.Fprintf(&builder, "After: %s\n", dossier.Scope.After.Format("2006-01-02"))
	}
	if dossier.Scope.Before != nil {
		fmt.Fprintf(&builder, "Before: %s\n", dossier.Scope.Before.Format("2006-01-02"))
	}

	// Evidence references.
	if len(dossier.EvidenceRefs) > 0 {
		fmt.Fprintf(&builder, "\n%d evidence reference(s). Cite the complete references shown.\n", len(dossier.EvidenceRefs))
		for _, ref := range dossier.EvidenceRefs {
			fmt.Fprintf(&builder, "  [%s] document %s\n", ref.FullRef, ref.DocumentID[:min(8, len(ref.DocumentID))])
		}
	}

	// Participants.
	if len(dossier.Participants) > 0 {
		fmt.Fprintf(&builder, "\n%d participant(s) observed:\n", len(dossier.Participants))
		for _, p := range dossier.Participants {
			fmt.Fprintf(&builder, "  %s (%d message(s), %s to %s)\n",
				p.Address, p.MessageCount, p.EarliestDate[:min(10, len(p.EarliestDate))], p.LatestDate[:min(10, len(p.LatestDate))])
			if p.MostRecentStatement != nil {
				fmt.Fprintf(&builder, "    Most recent: [%s] %s\n", p.MostRecentStatement.Reference.FullRef,
					truncate(p.MostRecentStatement.Position, 100))
			}
		}
	}

	// Conversations.
	if len(dossier.Conversations) > 0 {
		fmt.Fprintf(&builder, "\n%d conversation(s) observed:\n", len(dossier.Conversations))
		for _, c := range dossier.Conversations {
			fmt.Fprintf(&builder, "  %s: %s (%d message(s))\n", c.ConversationID, c.Subject, c.MessageCount)
		}
	}

	// Timeline.
	if len(dossier.Timeline) > 0 {
		fmt.Fprintf(&builder, "\nChronological timeline (%d event(s)):\n", len(dossier.Timeline))
		for _, e := range dossier.Timeline {
			fmt.Fprintf(&builder, "  %s [%s] %s: %s\n",
				e.Date[:min(10, len(e.Date))], e.Reference.FullRef, e.Participant, truncate(e.Summary, 80))
		}
	}

	// Conflicts.
	if len(dossier.ConflictingStatements) > 0 {
		fmt.Fprintf(&builder, "\nConflicting statements:\n")
		for _, cs := range dossier.ConflictingStatements {
			fmt.Fprintf(&builder, "  %s:\n", cs.Participant)
			for _, s := range cs.Statements {
				fmt.Fprintf(&builder, "    [%s] %s\n", s.Reference.FullRef, truncate(s.Position, 100))
			}
		}
	}

	// Warnings.
	if len(dossier.Warnings) > 0 {
		fmt.Fprintf(&builder, "\nWarnings:\n")
		for _, w := range dossier.Warnings {
			fmt.Fprintf(&builder, "  %s: %s\n", w.Kind, w.Message)
		}
	}

	// Completion.
	if dossier.Complete {
		builder.WriteString("\nDelivery complete: all evidence admitted by this result's aggregate budget has been delivered.\n")
	} else {
		fmt.Fprintf(&builder, "\nMORE EVIDENCE AVAILABLE. Call review_awaiting_reply with exactly {\"cursor\":%q}.\n", dossier.NextCursor)
	}

	return builder.String()
}

// renderReviewDossier produces the readable text representation of a ReviewDossier.
func renderReviewDossier(dossier *analysis.ReviewDossier) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Outstanding items review: %d candidate(s), %d returned\n", dossier.TotalCandidates, dossier.ReturnedCount)

	if len(dossier.Classifications) > 0 {
		for i, c := range dossier.Classifications {
			fmt.Fprintf(&builder, "\n[%d] %s — %s\n", i+1, c.ConversationID, c.Subject)
			fmt.Fprintf(&builder, "Classification: %s\n", c.Classification)
			fmt.Fprintf(&builder, "Reasoning: %s\n", c.Reasoning)
			if len(c.EvidenceRefs) > 0 {
				refs := make([]string, len(c.EvidenceRefs))
				for j, ref := range c.EvidenceRefs {
					refs[j] = ref.FullRef
				}
				fmt.Fprintf(&builder, "Evidence: %s\n", strings.Join(refs, ", "))
			}
		}
	}

	// Warnings.
	if len(dossier.Warnings) > 0 {
		fmt.Fprintf(&builder, "\nWarnings:\n")
		for _, w := range dossier.Warnings {
			fmt.Fprintf(&builder, "  %s: %s\n", w.Kind, w.Message)
		}
	}

	if dossier.Complete {
		builder.WriteString("\nDelivery complete.\n")
	}

	return builder.String()
}

// truncate shortens a string to the given maximum length.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

// parseFlexibleDate parses a date string in RFC 3339 or date-only format.
func parseFlexibleDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("date must be RFC 3339 or YYYY-MM-DD format")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// analyzeInputSchema returns the JSON Schema for the analyze_topic tool.
func analyzeInputSchema() map[string]any {
	return map[string]any{
		"$schema": jsonSchema202012, "type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"question": map[string]any{
				"type": "string", "minLength": 1, "maxLength": analysis.MaxTopicRunes,
				"description": "Research topic or question to investigate across the corpus.",
			},
			"participants": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"maxItems": analysis.MaxParticipants,
				"description": "Optional list of mailbox addresses or display names to focus on.",
			},
			"after": map[string]any{
				"type": []string{"string", "null"},
				"description": "Restrict to messages on or after this date (RFC 3339 or YYYY-MM-DD).",
			},
			"before": map[string]any{
				"type": []string{"string", "null"},
				"description": "Restrict to messages on or before this date (RFC 3339 or YYYY-MM-DD).",
			},
			"conversations": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"maxItems": analysis.MaxConversations,
				"description": "Optional list of conversation or thread IDs to include.",
			},
			"evidence_budget": map[string]any{
				"type": "integer", "minimum": 0, "default": analysis.MaxEvidenceBudget,
				"description": "Total UTF-8 byte budget across all research passes.",
			},
			"cursor": map[string]any{
				"type": "string", "minLength": 1, "maxLength": maxCursorBytes,
				"description": "Opaque continuation cursor from a previous call.",
			},
		},
		"required": []string{"question"},
	}
}

// reviewInputSchema returns the JSON Schema for the review_awaiting_reply tool.
func reviewInputSchema() map[string]any {
	return map[string]any{
		"$schema": jsonSchema202012, "type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"candidate_ids": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"maxItems": analysis.MaxConversations,
				"description": "Optional list of specific conversation IDs to review.",
			},
			"max_items": map[string]any{
				"type": "integer", "minimum": 1, "default": 15,
				"description": "Maximum number of candidates to return.",
			},
			"cursor": map[string]any{
				"type": "string", "minLength": 1, "maxLength": maxCursorBytes,
				"description": "Opaque continuation cursor from a previous call.",
			},
		},
	}
}

// dossierOutputSchema returns the JSON Schema for the dossier output.
func dossierOutputSchema() map[string]any {
	return map[string]any{
		"$schema": jsonSchema202012, "type": "object", "additionalProperties": true,
	}
}

// reviewDossierOutputSchema returns the JSON Schema for the review dossier output.
func reviewDossierOutputSchema() map[string]any {
	return dossierOutputSchema()
}

// Ensure AnalysisTool implements the tool interface at compile time.
var _ Retriever = (*retrieval.Service)(nil)
