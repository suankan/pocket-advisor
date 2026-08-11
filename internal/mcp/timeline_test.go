package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/suankan/pocket-advisor/internal/topicgraph"
)

const (
	syntheticTimelineVersion = "11111111-1111-5111-8111-111111111111"
	syntheticTimelineMention = "22222222-2222-5222-8222-222222222222"
	syntheticTimelineLater   = "33333333-3333-5333-8333-333333333333"
	syntheticTimelineDoc     = "44444444-4444-5444-8444-444444444444"
)

type stubTimelineRetriever struct {
	result *topicgraph.TimelineResult
	err    error
	got    topicgraph.TimelineRequest
}

func (s *stubTimelineRetriever) Timeline(_ context.Context, request topicgraph.TimelineRequest) (*topicgraph.TimelineResult, error) {
	s.got = request
	return s.result, s.err
}

func syntheticDocumentReference(id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte("1d" + id))
}

func syntheticTimelineResult() *topicgraph.TimelineResult {
	at := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	ref := topicgraph.EncodeMentionReference(syntheticTimelineVersion, syntheticTimelineMention)
	later := topicgraph.EncodeMentionReference(syntheticTimelineVersion, syntheticTimelineLater)
	return &topicgraph.TimelineResult{
		GraphVersion: syntheticTimelineVersion,
		SnapshotAt:   at,
		Nodes: []topicgraph.TimelineNode{{
			MentionRef: ref, SentAt: &at,
			Evidence: []topicgraph.Citation{{DocumentRef: syntheticDocumentReference(syntheticTimelineDoc), StartByte: 5, EndByte: 12, NormalizedTextSHA256: strings.Repeat("a", 64), SliceSHA256: strings.Repeat("b", 64)}},
		}, {
			MentionRef: later, SentAt: ptrTime(at.Add(time.Hour)),
			Evidence: []topicgraph.Citation{{DocumentRef: syntheticDocumentReference(syntheticTimelineDoc), StartByte: 20, EndByte: 27, NormalizedTextSHA256: strings.Repeat("a", 64), SliceSHA256: strings.Repeat("c", 64)}},
		}},
		Relations:    []topicgraph.TimelineRelation{{EarlierMentionRef: ref, LaterMentionRef: later, Type: topicgraph.RelationAddresses, Confidence: .8}},
		Warnings:     []string{topicgraph.WarnTimelineDepthLimit},
		OmittedNodes: 1,
		Budget:       topicgraph.TimelineBudget{NodesUsed: 2, NodesAllowed: 8, BytesUsed: 14, BytesAllowed: 1024},
	}
}

func ptrTime(value time.Time) *time.Time { return &value }

func timelineCall(t *testing.T, tool *QueryTool, arguments any) (CallToolResult, error) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"name": "topic_timeline", "arguments": arguments})
	if err != nil {
		t.Fatal(err)
	}
	return tool.Call(context.Background(), raw)
}

func TestTopicTimelineUsesOnlyBoundedOpaqueInputs(t *testing.T) {
	stub := &stubTimelineRetriever{result: syntheticTimelineResult()}
	tool := &QueryTool{Service: &stubRetriever{result: syntheticResult()}, Workspace: "synthetic", Timeline: &TimelineTool{Service: stub, Workspace: "synthetic"}}
	ref := topicgraph.EncodeMentionReference(syntheticTimelineVersion, syntheticTimelineMention)
	result, err := timelineCall(t, tool, map[string]any{"ref": ref, "direction": "forward", "depth": 2, "max_nodes": 7, "max_bytes": 900})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected result error: %#v", result)
	}
	if got := stub.got.Limits; got.BackwardDepth != 0 || got.ForwardDepth != 2 || got.MaxNodes != 7 || got.MaxBytes != 900 || got.MaxLatency != topicgraph.DefaultTimelineLimits().MaxLatency {
		t.Fatalf("limits = %+v", got)
	}
	page, ok := result.StructuredContent.(*timelineMCPResult)
	if !ok || page.Kind != "topic_timeline" || page.Timeline != stub.result || page.ResponseBudget.Unit != utf8ByteUnit {
		t.Fatalf("timeline result = %#v", result.StructuredContent)
	}
	if page.Timeline.Budget.NodesUsed != 2 || len(page.Timeline.Nodes[0].Evidence) != 1 || page.Timeline.Warnings[0] != topicgraph.WarnTimelineDepthLimit || page.Timeline.OmittedNodes != 1 {
		t.Fatalf("missing typed source-backed timeline fields: %#v", page.Timeline)
	}
	if !strings.Contains(result.Content[0].Text, "source ["+page.Timeline.Nodes[0].Evidence[0].DocumentRef+"]") || !strings.Contains(result.Content[0].Text, "Derived chronological relations") || strings.Contains(result.Content[0].Text, "normalized_text") {
		t.Fatalf("unsafe or incomplete readable result: %q", result.Content[0].Text)
	}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > targetToolResultBytes || readableLines(result.Content[0].Text) > targetReadableLines {
		t.Fatalf("unbounded result: bytes=%d lines=%d err=%v", len(encoded), readableLines(result.Content[0].Text), err)
	}
	value := map[string]any{}
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	structured := value["structuredContent"]
	if err := compileJSONSchema(t, "topic-timeline-output.schema.json", timelineOutputSchema()).Validate(structured); err != nil {
		t.Fatalf("output schema rejected result: %v", err)
	}
}

func TestTopicTimelineRejectsRawScopeAndBoundsBeforeService(t *testing.T) {
	stub := &stubTimelineRetriever{result: syntheticTimelineResult()}
	tool := &QueryTool{Service: &stubRetriever{result: syntheticResult()}, Workspace: "synthetic", Timeline: &TimelineTool{Service: stub, Workspace: "synthetic"}}
	documentRef := syntheticDocumentReference(syntheticTimelineDoc)
	for name, args := range map[string]any{
		"document citation cannot seed": map[string]any{"ref": documentRef},
		"workspace selector":            map[string]any{"ref": topicgraph.EncodeMentionReference(syntheticTimelineVersion, syntheticTimelineMention), "workspace": "other"},
		"raw byte range":                map[string]any{"ref": topicgraph.EncodeMentionReference(syntheticTimelineVersion, syntheticTimelineMention), "start_byte": 0},
		"graph version selector":        map[string]any{"ref": topicgraph.EncodeMentionReference(syntheticTimelineVersion, syntheticTimelineMention), "graph_version": syntheticTimelineVersion},
		"invalid direction":             map[string]any{"ref": topicgraph.EncodeMentionReference(syntheticTimelineVersion, syntheticTimelineMention), "direction": "sideways"},
		"unbounded nodes":               map[string]any{"ref": topicgraph.EncodeMentionReference(syntheticTimelineVersion, syntheticTimelineMention), "max_nodes": maxMCPTimelineNodes + 1},
		"unbounded bytes":               map[string]any{"ref": topicgraph.EncodeMentionReference(syntheticTimelineVersion, syntheticTimelineMention), "max_bytes": maxMCPTimelineBytes + 1},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := timelineCall(t, tool, args)
			var argument *argumentError
			if !errors.As(err, &argument) {
				t.Fatalf("error = %T %v, want argument error", err, err)
			}
		})
	}
	if stub.got.Reference != "" {
		t.Fatalf("service was called for rejected input: %+v", stub.got)
	}
}

func TestTopicTimelineDefinitionIsClosedAndUsesGenericName(t *testing.T) {
	tool := &QueryTool{Service: &stubRetriever{result: syntheticResult()}, Workspace: "synthetic", Timeline: &TimelineTool{Service: &stubTimelineRetriever{result: syntheticTimelineResult()}, Workspace: "synthetic"}}
	var definition *ToolDefinition
	for _, candidate := range tool.DescribeAll() {
		if candidate.Name == "topic_timeline" {
			copy := candidate
			definition = &copy
			break
		}
	}
	if definition == nil || definition.Annotations != readOnlyAnnotations() {
		t.Fatalf("definition = %#v", definition)
	}
	schema := compileJSONSchema(t, "topic-timeline-input.schema.json", definition.InputSchema)
	ref := topicgraph.EncodeEpisodeReference(syntheticTimelineVersion, syntheticTimelineMention)
	if err := schema.Validate(map[string]any{"ref": ref, "direction": "both", "depth": 0, "max_nodes": 1, "max_bytes": 1}); err != nil {
		t.Fatalf("valid bounded request rejected: %v", err)
	}
	for name, args := range map[string]any{
		"workspace": map[string]any{"ref": ref, "workspace": "other"},
		"document":  map[string]any{"ref": ref, "document_id": syntheticTimelineDoc},
		"version":   map[string]any{"ref": ref, "graph_version": syntheticTimelineVersion},
		"range":     map[string]any{"ref": ref, "start_byte": 3},
		"depth":     map[string]any{"ref": ref, "depth": topicgraph.AbsoluteTimelineDepth + 1},
	} {
		if err := schema.Validate(args); err == nil {
			t.Errorf("%s unexpectedly passed", name)
		}
	}
}

func TestTopicTimelineUnavailableReferenceIsCorrectable(t *testing.T) {
	stub := &stubTimelineRetriever{err: topicgraph.ErrUnknownTimelineReference}
	tool := &QueryTool{Service: &stubRetriever{result: syntheticResult()}, Workspace: "synthetic", Timeline: &TimelineTool{Service: stub, Workspace: "synthetic"}}
	_, err := timelineCall(t, tool, map[string]any{"ref": topicgraph.EncodeMentionReference(syntheticTimelineVersion, syntheticTimelineMention)})
	var argument *argumentError
	if !errors.As(err, &argument) {
		t.Fatalf("error = %T %v, want correctable argument error", err, err)
	}
}
