package eval

import (
	"context"
	"errors"
	"math"

	"github.com/suankan/pocket-advisor/internal/topicgraph"
)

// TopicGraphEvaluationStore supplies only derived-graph aggregates and opaque
// traversal seeds. Neither source text nor graph identifiers are copied into
// a report.
type TopicGraphEvaluationStore interface {
	TopicGraphEvaluation(context.Context, int) (topicgraph.EvaluationData, error)
	EvaluateTopicTimeline(context.Context, string, string, topicgraph.TimelineLimits) (*topicgraph.TimelineResult, error)
}

// TopicGraphReport contains aggregate, private-safe graph evaluation results.
// It intentionally contains no graph, document, mention, episode, case, or
// workspace identifiers, labels, source ranges, or source content.
type TopicGraphReport struct {
	Available     bool                     `json:"available"`
	ActiveVersion bool                     `json:"active_version"`
	Mentions      TopicGraphCoverage       `json:"mention_coverage"`
	Edges         TopicGraphCoverage       `json:"edge_coverage"`
	Episodes      TopicGraphCoverage       `json:"episode_coverage"`
	Relations     TopicGraphRelationReport `json:"relations"`
	Timelines     TopicGraphTimelineReport `json:"timelines"`
	Gates         TopicGraphGateReport     `json:"gates"`
}

// TopicGraphCoverage gives a count and a denominator without identifying any
// contributing records.
type TopicGraphCoverage struct {
	Count    int     `json:"count"`
	Eligible int     `json:"eligible"`
	Covered  int     `json:"covered"`
	Rate     float64 `json:"rate"`
}

// TopicGraphRelationReport summarizes the closed relation vocabulary and
// classifier confidence distribution. Warnings are closed aggregate codes.
type TopicGraphRelationReport struct {
	Candidates    int                             `json:"candidates"`
	Supported     int                             `json:"supported"`
	Types         map[topicgraph.RelationType]int `json:"types"`
	Confidence    Distribution                    `json:"confidence"`
	Warnings      map[string]int                  `json:"warnings,omitempty"`
	TotalWarnings int                             `json:"total_warnings"`
}

// TopicGraphTimelineReport records only aggregate bounded traversal outcomes.
type TopicGraphTimelineReport struct {
	Attempted       int            `json:"attempted"`
	Valid           int            `json:"valid"`
	Invalid         int            `json:"invalid"`
	ValidityRate    float64        `json:"validity_rate"`
	NodesUsed       int            `json:"nodes_used"`
	NodesAllowed    int            `json:"nodes_allowed"`
	BytesUsed       int            `json:"bytes_used"`
	BytesAllowed    int            `json:"bytes_allowed"`
	OmittedNodes    int            `json:"omitted_nodes"`
	WarningCounts   map[string]int `json:"warning_counts,omitempty"`
	BudgetTruncated int            `json:"budget_truncated"`
}

// TopicGraphGateReport makes configured mandatory gates explainable without
// leaking which graph record caused a failure.
type TopicGraphGateReport struct {
	Configured bool     `json:"configured"`
	Mandatory  bool     `json:"mandatory"`
	Passed     bool     `json:"passed"`
	Failures   []string `json:"failures,omitempty"`
}

// TopicGraphThresholds is optional because graphs remain a derived layer. A
// configured mandatory set turns its non-zero requirements into run-level
// gates; an unconfigured graph remains informational.
type TopicGraphThresholds struct {
	Mandatory             bool    `json:"mandatory"`
	MinMentionCoverage    float64 `json:"min_mention_coverage"`
	MinEdgeCoverage       float64 `json:"min_edge_coverage"`
	MinEpisodeCoverage    float64 `json:"min_episode_coverage"`
	MinRelationConfidence float64 `json:"min_relation_confidence"`
	MinTimelineValidity   float64 `json:"min_timeline_validity"`
	MaxTimelineOmissions  int     `json:"max_timeline_omissions"`
	MaxWarnings           int     `json:"max_warnings"`
}

const topicGraphTimelineSampleLimit = 64

// EvaluateTopicGraph converts private graph inputs into a report-safe summary
// and probes a bounded deterministic sample of active-graph timelines.
func EvaluateTopicGraph(ctx context.Context, store TopicGraphEvaluationStore) (*TopicGraphReport, error) {
	if store == nil {
		return &TopicGraphReport{}, nil
	}
	data, err := store.TopicGraphEvaluation(ctx, topicGraphTimelineSampleLimit)
	if err != nil {
		return nil, err
	}
	report := &TopicGraphReport{Available: data.Available, ActiveVersion: data.ActiveVersionID != ""}
	report.Mentions = coverage(data.Mentions, data.EligibleDocuments, data.MentionDocuments)
	report.Edges = coverage(data.Edges, data.RelationCandidates, data.Edges)
	report.Episodes = coverage(data.Episodes, data.Mentions, data.EpisodeMemberships)
	report.Relations = TopicGraphRelationReport{
		Candidates: data.RelationCandidates,
		Supported:  data.Edges,
		Types:      map[topicgraph.RelationType]int{},
		Confidence: DistributionFrom(data.RelationConfidences),
		Warnings:   copyKnownCounts(data.Warnings, knownRelationWarnings),
	}
	for _, kind := range []topicgraph.RelationType{topicgraph.RelationAddresses, topicgraph.RelationContinues, topicgraph.RelationElaborates, topicgraph.RelationContradicts, topicgraph.RelationStatesResolution, topicgraph.RelationPossiblyRelated} {
		report.Relations.Types[kind] = data.RelationTypes[kind]
	}
	for _, n := range report.Relations.Warnings {
		report.Relations.TotalWarnings += n
	}
	if !report.ActiveVersion {
		return report, nil
	}

	limits := topicgraph.DefaultTimelineLimits()
	report.Timelines.ValidityRate = 1
	for _, mentionID := range data.TimelineMentionSeedIDs {
		report.Timelines.Attempted++
		result, err := store.EvaluateTopicTimeline(ctx, data.ActiveVersionID, mentionID, limits)
		if err != nil || !validTopicGraphTimeline(result, limits) {
			report.Timelines.Invalid++
			addCount(&report.Timelines.WarningCounts, "topic_timeline_invalid")
			continue
		}
		report.Timelines.Valid++
		report.Timelines.NodesUsed += result.Budget.NodesUsed
		report.Timelines.NodesAllowed += result.Budget.NodesAllowed
		report.Timelines.BytesUsed += result.Budget.BytesUsed
		report.Timelines.BytesAllowed += result.Budget.BytesAllowed
		report.Timelines.OmittedNodes += result.OmittedNodes
		truncated := false
		for _, warning := range result.Warnings {
			if !knownTimelineWarnings[warning] {
				addCount(&report.Timelines.WarningCounts, "topic_timeline_unknown_warning")
				continue
			}
			addCount(&report.Timelines.WarningCounts, warning)
			if warning == topicgraph.WarnTimelineNodeLimit || warning == topicgraph.WarnTimelineByteLimit {
				truncated = true
			}
		}
		if truncated {
			report.Timelines.BudgetTruncated++
		}
	}
	if report.Timelines.Attempted > 0 {
		report.Timelines.ValidityRate = float64(report.Timelines.Valid) / float64(report.Timelines.Attempted)
	}
	return report, nil
}

func coverage(count, eligible, covered int) TopicGraphCoverage {
	if count < 0 {
		count = 0
	}
	if eligible < 0 {
		eligible = 0
	}
	if covered < 0 {
		covered = 0
	}
	r := TopicGraphCoverage{Count: count, Eligible: eligible, Covered: covered, Rate: 1}
	if eligible > 0 {
		r.Rate = float64(covered) / float64(eligible)
	}
	return r
}

func validTopicGraphTimeline(result *topicgraph.TimelineResult, limits topicgraph.TimelineLimits) bool {
	if result == nil || result.GraphVersion == "" || result.Budget.NodesUsed < 0 || result.Budget.BytesUsed < 0 || result.Budget.NodesAllowed != limits.MaxNodes || result.Budget.BytesAllowed != limits.MaxBytes || result.Budget.NodesUsed > limits.MaxNodes || result.Budget.BytesUsed > limits.MaxBytes || result.OmittedNodes < 0 {
		return false
	}
	return true
}

var knownRelationWarnings = map[string]bool{
	"unsupported_relation_candidate": true,
}
var knownTimelineWarnings = map[string]bool{
	topicgraph.WarnTimelineNodeLimit: true, topicgraph.WarnTimelineByteLimit: true,
	topicgraph.WarnTimelineDepthLimit: true, topicgraph.WarnTimelineCycle: true,
	topicgraph.WarnTimelineEdgeLimit: true, topicgraph.WarnTimelineEvidence: true,
}

func copyKnownCounts(in map[string]int, known map[string]bool) map[string]int {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]int, len(in))
	for key, value := range in {
		if known[key] && value > 0 {
			out[key] = value
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
func addCount(counts *map[string]int, key string) {
	if *counts == nil {
		*counts = make(map[string]int)
	}
	(*counts)[key]++
}

func applyTopicGraphThresholds(summary *Summary, report *TopicGraphReport, thresholds ThresholdsConfig) {
	if thresholds.TopicGraph == nil {
		return
	}
	gate := thresholds.TopicGraph
	report.Gates.Configured, report.Gates.Mandatory, report.Gates.Passed = true, gate.Mandatory, true
	fail := func(condition bool, code string) {
		if condition {
			report.Gates.Passed = false
			report.Gates.Failures = append(report.Gates.Failures, code)
		}
	}
	fail(!report.ActiveVersion, "active_graph_required")
	if report.ActiveVersion {
		fail(report.Mentions.Rate < gate.MinMentionCoverage, "mention_coverage")
		fail(report.Edges.Rate < gate.MinEdgeCoverage, "edge_coverage")
		fail(report.Episodes.Rate < gate.MinEpisodeCoverage, "episode_coverage")
		if report.Relations.Confidence.N > 0 {
			fail(report.Relations.Confidence.Mean < gate.MinRelationConfidence, "relation_confidence")
		}
		fail(report.Timelines.ValidityRate < gate.MinTimelineValidity, "timeline_validity")
		fail(report.Timelines.OmittedNodes > gate.MaxTimelineOmissions, "timeline_omissions")
		fail(report.Relations.TotalWarnings+sumCounts(report.Timelines.WarningCounts) > gate.MaxWarnings, "warnings")
	}
	if gate.Mandatory && !report.Gates.Passed {
		summary.OverallPassed = false
	}
}

func sumCounts(values map[string]int) int {
	n := 0
	for _, value := range values {
		n += value
	}
	return n
}

// ValidateTopicGraphThresholds rejects unsafe numeric values before they can
// silently disable a mandatory graph quality gate.
func ValidateTopicGraphThresholds(t *TopicGraphThresholds) error {
	if t == nil {
		return nil
	}
	for _, value := range []float64{t.MinMentionCoverage, t.MinEdgeCoverage, t.MinEpisodeCoverage, t.MinRelationConfidence, t.MinTimelineValidity} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
			return errors.New("topic graph coverage and confidence thresholds must be between 0 and 1")
		}
	}
	if t.MaxTimelineOmissions < 0 || t.MaxWarnings < 0 {
		return errors.New("topic graph maximum thresholds cannot be negative")
	}
	return nil
}
