package eval

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/suankan/pocket-advisor/internal/topicgraph"
)

type syntheticTopicGraphStore struct {
	data    topicgraph.EvaluationData
	results map[string]*topicgraph.TimelineResult
	errs    map[string]error
}

func (s syntheticTopicGraphStore) TopicGraphEvaluation(context.Context, int) (topicgraph.EvaluationData, error) {
	return s.data, nil
}
func (s syntheticTopicGraphStore) EvaluateTopicTimeline(_ context.Context, _, mentionID string, _ topicgraph.TimelineLimits) (*topicgraph.TimelineResult, error) {
	if err := s.errs[mentionID]; err != nil {
		return nil, err
	}
	return s.results[mentionID], nil
}

func validSyntheticTimeline(nodes, bytes, omitted int, warnings ...string) *topicgraph.TimelineResult {
	limits := topicgraph.DefaultTimelineLimits()
	return &topicgraph.TimelineResult{
		GraphVersion: "internal-version-only",
		Warnings:     warnings, OmittedNodes: omitted,
		Budget: topicgraph.TimelineBudget{NodesUsed: nodes, NodesAllowed: limits.MaxNodes, BytesUsed: bytes, BytesAllowed: limits.MaxBytes},
	}
}

func TestEvaluateTopicGraphAggregatesWithoutPrivateReferences(t *testing.T) {
	const privateVersion = "77777777-7777-5777-8777-777777777777"
	const privateMention = "88888888-8888-5888-8888-888888888888"
	store := syntheticTopicGraphStore{
		data: topicgraph.EvaluationData{
			Available: true, ActiveVersionID: privateVersion,
			EligibleDocuments: 4, MentionDocuments: 3, Mentions: 5,
			RelationCandidates: 4, Edges: 3, Episodes: 2, EpisodeMemberships: 4,
			RelationTypes:          map[topicgraph.RelationType]int{topicgraph.RelationAddresses: 2, topicgraph.RelationContradicts: 1, topicgraph.RelationPossiblyRelated: 1},
			RelationConfidences:    []float64{0.9, 0.8, 0.7, 0.2},
			Warnings:               map[string]int{"unsupported_relation_candidate": 1},
			TimelineMentionSeedIDs: []string{privateMention, "99999999-9999-5999-8999-999999999999"},
		},
		results: map[string]*topicgraph.TimelineResult{privateMention: validSyntheticTimeline(2, 10, 1, topicgraph.WarnTimelineNodeLimit)},
		errs:    map[string]error{"99999999-9999-5999-8999-999999999999": errors.New("synthetic failure")},
	}
	report, err := EvaluateTopicGraph(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Available || !report.ActiveVersion || report.Mentions.Rate != .75 || report.Edges.Rate != .75 || report.Episodes.Rate != .8 {
		t.Fatalf("coverage = %+v / %+v / %+v", report.Mentions, report.Edges, report.Episodes)
	}
	if report.Relations.Types[topicgraph.RelationAddresses] != 2 || report.Relations.Confidence.N != 4 || report.Relations.TotalWarnings != 1 {
		t.Fatalf("relations = %+v", report.Relations)
	}
	if report.Timelines.Attempted != 2 || report.Timelines.Valid != 1 || report.Timelines.Invalid != 1 || report.Timelines.OmittedNodes != 1 || report.Timelines.BudgetTruncated != 1 {
		t.Fatalf("timelines = %+v", report.Timelines)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{privateVersion, privateMention, "99999999-9999-5999-8999-999999999999"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("private reference %q leaked in %s", private, raw)
		}
	}
}

func TestTopicGraphMandatoryGatesControlOverallResult(t *testing.T) {
	report := &TopicGraphReport{
		Available: true, ActiveVersion: true,
		Mentions: TopicGraphCoverage{Rate: .5}, Edges: TopicGraphCoverage{Rate: 1}, Episodes: TopicGraphCoverage{Rate: 1},
		Relations: TopicGraphRelationReport{Confidence: Distribution{Mean: .9, N: 1}},
		Timelines: TopicGraphTimelineReport{ValidityRate: 1},
	}
	thresholds := ThresholdsConfig{TopicGraph: &TopicGraphThresholds{Mandatory: true, MinMentionCoverage: .75}}
	summary := Summary{OverallPassed: true}
	applyTopicGraphThresholds(&summary, report, thresholds)
	if summary.OverallPassed || report.Gates.Passed || len(report.Gates.Failures) != 1 || report.Gates.Failures[0] != "mention_coverage" {
		t.Fatalf("mandatory gate = summary %+v report %+v", summary, report.Gates)
	}
	summary.OverallPassed = true
	thresholds.TopicGraph.Mandatory = false
	applyTopicGraphThresholds(&summary, report, thresholds)
	if !summary.OverallPassed || report.Gates.Passed {
		t.Fatalf("informational gate = summary %+v report %+v", summary, report.Gates)
	}
}

func TestTopicGraphThresholdValidation(t *testing.T) {
	if err := ValidateTopicGraphThresholds(&TopicGraphThresholds{MinTimelineValidity: 1.1}); err == nil {
		t.Fatal("accepted out-of-range threshold")
	}
	if err := ValidateTopicGraphThresholds(&TopicGraphThresholds{MaxWarnings: -1}); err == nil {
		t.Fatal("accepted negative maximum")
	}
}
