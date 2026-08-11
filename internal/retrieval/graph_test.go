package retrieval

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/suankan/pocket-advisor/internal/topicgraph"
)

const graphTestDocumentID = "00000000-0000-0000-0000-000000000101"

func graphTestCitation(text string) topicgraph.Citation {
	full := sha256.Sum256([]byte(text))
	slice := sha256.Sum256([]byte(text[:2])) // "é" is one rune, two UTF-8 bytes.
	return topicgraph.Citation{
		DocumentRef:          base64.RawURLEncoding.EncodeToString([]byte("1d" + graphTestDocumentID)),
		StartByte:            0,
		EndByte:              2,
		NormalizedTextSHA256: hex.EncodeToString(full[:]),
		SliceSHA256:          hex.EncodeToString(slice[:]),
	}
}

func TestGraphExpansionIsDisabledWithoutAnExplicitGate(t *testing.T) {
	service := &Service{}
	graph, warnings := service.expandTopicGraph(context.Background(), []scored{{candidate: candidate{DocID: graphTestDocumentID, StartByte: 0, EndByte: 1}}}, newBudgeter(1))
	if graph != nil || len(warnings) != 0 {
		t.Fatalf("disabled expansion = %#v, %#v", graph, warnings)
	}
}

func TestPackGraphNodesChargesOnlyExactCitedSpans(t *testing.T) {
	text := "éx"
	citation := graphTestCitation(text)
	result := &topicgraph.TimelineResult{
		GraphVersion: "graph-v1",
		Nodes:        []topicgraph.TimelineNode{{MentionRef: "seed", Evidence: []topicgraph.Citation{citation}}},
		Warnings:     []string{topicgraph.WarnTimelineCycle},
	}
	budget := newBudgeter(8)
	if _, ok := budget.take("base"); !ok {
		t.Fatal("normal packet text did not fit")
	}

	out, warnings := packGraphNodes(result, map[string]Document{graphTestDocumentID: {DocID: graphTestDocumentID}}, map[string]string{graphTestDocumentID: text}, budget)
	if len(out.Nodes) != 1 || len(out.Nodes[0].Spans) != 1 {
		t.Fatalf("graph nodes = %#v", out.Nodes)
	}
	if got := out.Nodes[0].Spans[0].Text; got != "é" {
		t.Fatalf("span text = %q, want exact cited UTF-8 slice", got)
	}
	if budget.used != len("base")+len("é") || budget.remaining != 2 {
		t.Fatalf("budget = used %d remaining %d, want exact source-span charge", budget.used, budget.remaining)
	}
	if len(warnings) != 1 || warnings[0] != topicgraph.WarnTimelineCycle || out.OmittedNodes != 0 {
		t.Fatalf("warnings/omissions were not retained: %#v, %d", warnings, out.OmittedNodes)
	}
}

func TestPackGraphNodesOmitsOverBudgetNodesAndRelations(t *testing.T) {
	text := "éx"
	citation := graphTestCitation(text)
	result := &topicgraph.TimelineResult{
		Nodes:        []topicgraph.TimelineNode{{MentionRef: "seed", Evidence: []topicgraph.Citation{citation}}},
		Relations:    []topicgraph.TimelineRelation{{EarlierMentionRef: "seed", LaterMentionRef: "other"}},
		OmittedNodes: 2,
	}
	budget := newBudgeter(5)
	if _, ok := budget.take("base"); !ok {
		t.Fatal("normal packet text did not fit")
	}

	out, warnings := packGraphNodes(result, map[string]Document{graphTestDocumentID: {DocID: graphTestDocumentID}}, map[string]string{graphTestDocumentID: text}, budget)
	if len(out.Nodes) != 0 || len(out.Relations) != 0 || out.OmittedNodes != 3 {
		t.Fatalf("over-budget expansion = %#v", out)
	}
	if budget.used != len("base") || !budget.truncated {
		t.Fatalf("budget charged graph evidence that was omitted: %#v", budget)
	}
	if len(warnings) != 1 || warnings[0] != topicgraph.WarnTimelineByteLimit {
		t.Fatalf("warnings = %#v, want byte-limit warning", warnings)
	}
}

func TestGraphNodeRejectsInvalidCitationWithoutTextRepair(t *testing.T) {
	text := "éx"
	citation := graphTestCitation(text)
	citation.EndByte = 1 // middle of the UTF-8 rune
	if _, ok := graphNodeFromCitations(topicgraph.TimelineNode{MentionRef: "seed", Evidence: []topicgraph.Citation{citation}}, map[string]Document{graphTestDocumentID: {DocID: graphTestDocumentID}}, map[string]string{graphTestDocumentID: text}); ok {
		t.Fatal("accepted a citation that does not preserve a UTF-8 source boundary")
	}
}
