package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"

	"github.com/google/uuid"
	"github.com/suankan/pocket-advisor/internal/topicgraph"
)

const (
	maxTimelineReferenceBytes = 256
	maxMCPTimelineNodes       = topicgraph.DefaultTimelineMaxNodes
	maxMCPTimelineBytes       = topicgraph.DefaultTimelineMaxBytes
)

// TimelineRetriever is the transport-independent boundary for following a
// source-backed topic timeline. The implementation is fixed to one workspace;
// callers can supply only a server-issued mention or episode reference.
type TimelineRetriever interface {
	Timeline(context.Context, topicgraph.TimelineRequest) (*topicgraph.TimelineResult, error)
}

// TimelineTool adapts the versioned topic graph to MCP. It never accepts a
// document, source range, graph version, or workspace selector.
type TimelineTool struct {
	Service   TimelineRetriever
	Workspace string
	Title     string
	// Log traces every topic_timeline call. Nil-safe, see QueryTool.Log.
	Log *slog.Logger
}

func (t *TimelineTool) logger() *slog.Logger {
	if t.Log != nil {
		return t.Log
	}
	return slog.Default()
}

func (t *TimelineTool) Name() string { return "topic_timeline" }

func (t *TimelineTool) Describe() ToolDefinition {
	title := t.Title
	if title == "" {
		title = t.Workspace
	}
	return ToolDefinition{
		Name:  t.Name(),
		Title: "Follow " + title + " topic timeline",
		Description: "Follow a bounded chronological topic-evolution graph in this fixed workspace. " +
			"Pass only an opaque server-issued topic mention or episode reference returned as evidence by this tool. " +
			"Every node contains cited source spans; topic labels and relations are derived annotations, not source facts. " +
			"Report warnings and omitted nodes. Do not construct a reference or provide a workspace, graph version, document, byte range, credential, or arbitrary graph query.",
		InputSchema: timelineInputSchema(), OutputSchema: timelineOutputSchema(), Annotations: readOnlyAnnotations(),
	}
}

type timelineArguments struct {
	Ref       string  `json:"ref"`
	Direction *string `json:"direction,omitempty"`
	Depth     *int    `json:"depth,omitempty"`
	MaxNodes  *int    `json:"max_nodes,omitempty"`
	MaxBytes  *int    `json:"max_bytes,omitempty"`
}

// Call validates closed MCP inputs before invoking the fixed-workspace
// timeline service. QueryTool dispatches to this method only for this tool.
func (t *TimelineTool) Call(ctx context.Context, raw json.RawMessage) (CallToolResult, error) {
	if t == nil || t.Service == nil || strings.TrimSpace(t.Workspace) == "" {
		return CallToolResult{}, fmt.Errorf("topic timeline service is unavailable")
	}
	var params rawCallParams
	if err := decodeStrict(raw, &params); err != nil {
		return CallToolResult{}, &argumentError{message: "tools/call params must be a valid object"}
	}
	if params.Name != t.Name() {
		return CallToolResult{}, &unknownToolError{}
	}
	if len(params.Task) > 0 && string(params.Task) != "null" {
		return CallToolResult{}, &argumentError{message: "task-augmented execution is not supported"}
	}
	var args timelineArguments
	if err := decodeStrict(params.Arguments, &args); err != nil {
		return CallToolResult{}, &argumentError{message: "arguments must match the advertised topic-timeline input schema"}
	}
	if strings.TrimSpace(args.Ref) == "" || len(args.Ref) > maxTimelineReferenceBytes {
		return CallToolResult{}, &argumentError{message: "ref must be a bounded server-issued topic mention or episode reference"}
	}
	// Decode here as well as in TimelineService. This protects alternate
	// implementations of TimelineRetriever from accidentally accepting a
	// document citation or a client-made identifier.
	if _, err := topicgraph.DecodeTimelineReference(args.Ref); err != nil {
		return CallToolResult{}, &argumentError{message: "ref is invalid or unavailable in this workspace"}
	}
	limits, err := timelineLimits(args)
	if err != nil {
		return CallToolResult{}, err
	}
	result, err := t.Service.Timeline(ctx, topicgraph.TimelineRequest{Reference: args.Ref, Limits: limits})
	if err != nil {
		t.logger().Info("topic_timeline", "ref", args.Ref, "error", err.Error())
		return CallToolResult{}, timelineArgumentError(err)
	}
	t.logger().Info("topic_timeline", "ref", args.Ref, "nodes", len(result.Nodes),
		"relations", len(result.Relations), "omitted_nodes", result.OmittedNodes)
	return finalizeTimelineResult(result)
}

func timelineLimits(args timelineArguments) (topicgraph.TimelineLimits, error) {
	limits := topicgraph.DefaultTimelineLimits()
	direction := "both"
	if args.Direction != nil {
		direction = *args.Direction
		if direction != "both" && direction != "backward" && direction != "forward" {
			return topicgraph.TimelineLimits{}, &argumentError{message: "direction must be both, backward, or forward"}
		}
	}
	depth := topicgraph.DefaultTimelineBackwardDepth
	if args.Depth != nil {
		depth = *args.Depth
		if depth < 0 || depth > topicgraph.AbsoluteTimelineDepth {
			return topicgraph.TimelineLimits{}, &argumentError{message: fmt.Sprintf("depth must be between 0 and %d", topicgraph.AbsoluteTimelineDepth)}
		}
	}
	switch direction {
	case "backward":
		limits.BackwardDepth, limits.ForwardDepth = depth, 0
	case "forward":
		limits.BackwardDepth, limits.ForwardDepth = 0, depth
	default:
		limits.BackwardDepth, limits.ForwardDepth = depth, depth
	}
	if args.MaxNodes != nil {
		if *args.MaxNodes < 1 || *args.MaxNodes > maxMCPTimelineNodes {
			return topicgraph.TimelineLimits{}, &argumentError{message: fmt.Sprintf("max_nodes must be between 1 and %d", maxMCPTimelineNodes)}
		}
		limits.MaxNodes = *args.MaxNodes
	}
	if args.MaxBytes != nil {
		if *args.MaxBytes < 1 || *args.MaxBytes > maxMCPTimelineBytes {
			return topicgraph.TimelineLimits{}, &argumentError{message: fmt.Sprintf("max_bytes must be between 1 and %d", maxMCPTimelineBytes)}
		}
		limits.MaxBytes = *args.MaxBytes
	}
	return limits, nil
}

func timelineArgumentError(err error) error {
	if errors.Is(err, topicgraph.ErrUnknownTimelineReference) || errors.Is(err, topicgraph.ErrInvalidTimelineRequest) {
		return &argumentError{message: "topic reference or traversal bounds are invalid or unavailable in this workspace"}
	}
	return err
}

// timelineMCPResult is the successful, typed MCP envelope. TimelineResult
// includes only opaque references and source-span hashes/offsets, never source
// body text or a client-selected storage address.
type timelineMCPResult struct {
	Kind           string                     `json:"kind"`
	Timeline       *topicgraph.TimelineResult `json:"timeline"`
	ResponseBudget ResponseBudget             `json:"response_budget"`
}

func finalizeTimelineResult(timeline *topicgraph.TimelineResult) (CallToolResult, error) {
	if err := validateTimelineResult(timeline); err != nil {
		return CallToolResult{}, fmt.Errorf("validate topic timeline result: %w", err)
	}
	page := &timelineMCPResult{Kind: "topic_timeline", Timeline: timeline, ResponseBudget: ResponseBudget{Allowed: targetToolResultBytes, Unit: utf8ByteUnit}}
	var result CallToolResult
	for range 4 {
		result = CallToolResult{Content: []TextContent{{Type: "text", Text: renderTimelineResult(page)}}, StructuredContent: page}
		encoded, err := json.Marshal(result)
		if err != nil {
			return CallToolResult{}, fmt.Errorf("encode topic timeline result: %w", err)
		}
		if page.ResponseBudget.Used == len(encoded) {
			break
		}
		page.ResponseBudget.Used = len(encoded)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return CallToolResult{}, fmt.Errorf("encode topic timeline result: %w", err)
	}
	if len(encoded) > targetToolResultBytes {
		return CallToolResult{}, &resultSizeError{limit: targetToolResultBytes}
	}
	if readableLines(result.Content[0].Text) > targetReadableLines {
		return CallToolResult{}, &resultLineError{limit: targetReadableLines}
	}
	return result, nil
}

func validateTimelineResult(result *topicgraph.TimelineResult) error {
	if result == nil || result.GraphVersion == "" || result.SnapshotAt.IsZero() || result.Nodes == nil || result.Relations == nil || result.Warnings == nil || result.OmittedNodes < 0 {
		return fmt.Errorf("incomplete timeline result")
	}
	budget := result.Budget
	if budget.NodesUsed < 0 || budget.NodesAllowed < 0 || budget.NodesUsed > budget.NodesAllowed || budget.BytesUsed < 0 || budget.BytesAllowed < 0 || budget.BytesUsed > budget.BytesAllowed {
		return fmt.Errorf("invalid timeline budget")
	}
	for _, node := range result.Nodes {
		ref, err := topicgraph.DecodeTimelineReference(node.MentionRef)
		if err != nil || ref.Kind != topicgraph.TimelineMentionRef || ref.VersionID != result.GraphVersion || len(node.Evidence) == 0 {
			return fmt.Errorf("invalid timeline node")
		}
		for _, citation := range node.Evidence {
			if !validTimelineDocumentReference(citation.DocumentRef) || citation.StartByte < 0 || citation.EndByte <= citation.StartByte || len(citation.NormalizedTextSHA256) != 64 || len(citation.SliceSHA256) != 64 {
				return fmt.Errorf("invalid timeline citation")
			}
		}
	}
	for _, relation := range result.Relations {
		before, beforeErr := topicgraph.DecodeTimelineReference(relation.EarlierMentionRef)
		after, afterErr := topicgraph.DecodeTimelineReference(relation.LaterMentionRef)
		if beforeErr != nil || afterErr != nil || before.Kind != topicgraph.TimelineMentionRef || after.Kind != topicgraph.TimelineMentionRef || before.VersionID != result.GraphVersion || after.VersionID != result.GraphVersion || !validTimelineRelation(relation.Type) || math.IsNaN(relation.Confidence) || math.IsInf(relation.Confidence, 0) || relation.Confidence < 0 || relation.Confidence > 1 {
			return fmt.Errorf("invalid timeline relation")
		}
	}
	return nil
}

func validTimelineDocumentReference(value string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) != 38 || string(raw[:2]) != "1d" {
		return false
	}
	_, err = uuid.Parse(string(raw[2:]))
	return err == nil
}

func validTimelineRelation(value topicgraph.RelationType) bool {
	switch value {
	case topicgraph.RelationAddresses, topicgraph.RelationContinues, topicgraph.RelationElaborates, topicgraph.RelationContradicts, topicgraph.RelationStatesResolution, topicgraph.RelationPossiblyRelated:
		return true
	default:
		return false
	}
}

func renderTimelineResult(page *timelineMCPResult) string {
	var b strings.Builder
	result := page.Timeline
	fmt.Fprintf(&b, "Topic timeline: graph version %s, snapshot %s.\n", result.GraphVersion, result.SnapshotAt.Format("2006-01-02T15:04:05Z07:00"))
	fmt.Fprintf(&b, "%d chronological topic mention(s). Cite the opaque mention and document references with the UTF-8 byte span.\n", len(result.Nodes))
	for _, node := range result.Nodes {
		if node.SentAt == nil {
			fmt.Fprintf(&b, "[%s] undated\n", node.MentionRef)
		} else {
			fmt.Fprintf(&b, "[%s] %s\n", node.MentionRef, node.SentAt.Format("2006-01-02T15:04:05Z07:00"))
		}
		for _, citation := range node.Evidence {
			fmt.Fprintf(&b, "  source [%s], UTF-8 bytes %d-%d\n", citation.DocumentRef, citation.StartByte, citation.EndByte)
		}
	}
	if len(result.Relations) > 0 {
		b.WriteString("Derived chronological relations (not source facts):\n")
		for _, relation := range result.Relations {
			fmt.Fprintf(&b, "  [%s] --%s (confidence %.3f)--> [%s]\n", relation.EarlierMentionRef, relation.Type, relation.Confidence, relation.LaterMentionRef)
		}
	}
	if len(result.Warnings) > 0 {
		fmt.Fprintf(&b, "Warnings: %s\n", strings.Join(result.Warnings, ", "))
	}
	if result.OmittedNodes > 0 {
		fmt.Fprintf(&b, "Omitted nodes: %d due to the traversal bounds.\n", result.OmittedNodes)
	}
	fmt.Fprintf(&b, "Timeline budget: %d of %d nodes; %d of %d UTF-8 source bytes. Response budget: %d of %d UTF-8 bytes.\n", result.Budget.NodesUsed, result.Budget.NodesAllowed, result.Budget.BytesUsed, result.Budget.BytesAllowed, page.ResponseBudget.Used, page.ResponseBudget.Allowed)
	return b.String()
}

func timelineInputSchema() map[string]any {
	return map[string]any{
		"$schema": jsonSchema202012, "type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"ref":       map[string]any{"type": "string", "minLength": 1, "maxLength": maxTimelineReferenceBytes, "description": "Opaque mention or episode reference returned by the server."},
			"direction": map[string]any{"enum": []string{"both", "backward", "forward"}, "default": "both"},
			"depth":     map[string]any{"type": "integer", "minimum": 0, "maximum": topicgraph.AbsoluteTimelineDepth, "default": topicgraph.DefaultTimelineBackwardDepth},
			"max_nodes": map[string]any{"type": "integer", "minimum": 1, "maximum": maxMCPTimelineNodes, "default": topicgraph.DefaultTimelineMaxNodes},
			"max_bytes": map[string]any{"type": "integer", "minimum": 1, "maximum": maxMCPTimelineBytes, "default": topicgraph.DefaultTimelineMaxBytes, "description": "Aggregate cited UTF-8 source-span bytes; this never requests raw source text."},
		},
		"required": []string{"ref"},
	}
}

func timelineOutputSchema() map[string]any {
	citation := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"document_ref": map[string]any{"type": "string", "minLength": 1}, "start_byte": map[string]any{"type": "integer", "minimum": 0}, "end_byte": map[string]any{"type": "integer", "minimum": 1}, "normalized_text_sha256": map[string]any{"type": "string", "minLength": 64, "maxLength": 64}, "slice_sha256": map[string]any{"type": "string", "minLength": 64, "maxLength": 64},
	}, "required": []string{"document_ref", "start_byte", "end_byte", "normalized_text_sha256", "slice_sha256"}}
	node := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"mention_ref": map[string]any{"type": "string", "minLength": 1}, "sent_at": map[string]any{"type": "string", "format": "date-time"}, "evidence": map[string]any{"type": "array", "items": citation},
	}, "required": []string{"mention_ref", "evidence"}}
	relation := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"earlier_mention_ref": map[string]any{"type": "string", "minLength": 1}, "later_mention_ref": map[string]any{"type": "string", "minLength": 1}, "type": map[string]any{"enum": []string{"addresses", "continues", "elaborates", "contradicts", "states_resolution", "possibly_related"}}, "confidence": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
	}, "required": []string{"earlier_mention_ref", "later_mention_ref", "type", "confidence"}}
	budget := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"nodes_used": map[string]any{"type": "integer", "minimum": 0}, "nodes_allowed": map[string]any{"type": "integer", "minimum": 0}, "bytes_used": map[string]any{"type": "integer", "minimum": 0}, "bytes_allowed": map[string]any{"type": "integer", "minimum": 0},
	}, "required": []string{"nodes_used", "nodes_allowed", "bytes_used", "bytes_allowed"}}
	timeline := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"graph_version": map[string]any{"type": "string", "minLength": 1}, "snapshot_at": map[string]any{"type": "string", "format": "date-time"}, "nodes": map[string]any{"type": "array", "items": node}, "relations": map[string]any{"type": "array", "items": relation}, "warnings": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "omitted_nodes": map[string]any{"type": "integer", "minimum": 0}, "budget": budget,
	}, "required": []string{"graph_version", "snapshot_at", "nodes", "relations", "warnings", "omitted_nodes", "budget"}}
	return map[string]any{"$schema": jsonSchema202012, "type": "object", "additionalProperties": false, "properties": map[string]any{
		"kind": map[string]any{"const": "topic_timeline"}, "timeline": timeline, "response_budget": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"used": map[string]any{"type": "integer", "minimum": 1}, "allowed": map[string]any{"const": targetToolResultBytes}, "unit": map[string]any{"const": utf8ByteUnit}}, "required": []string{"used", "allowed", "unit"}},
	}, "required": []string{"kind", "timeline", "response_budget"}}
}
