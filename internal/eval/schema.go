// Package eval provides a transport-independent retrieval evaluation workflow.
//
// It measures dense, lexical, fused, reranked, and selected retrieval stages
// against versioned evaluation cases. The CLI and MCP adapters are thin
// boundaries over this package (retrieval-design.md section 7).
package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// CaseSchemaVersion is the current evaluation case format version.
const CaseSchemaVersion = 1

// Category constants for case classification.
const (
	CatExactIdentifier = "exact-identifier"
	CatParaphrase      = "paraphrase"
	CatBilingual       = "bilingual"
	CatMultiTopic      = "multi-topic"
	CatThread          = "thread"
	CatAttachment      = "attachment"
	CatOffDomain       = "off-domain"
)

// Case is a single evaluation case. Stable IDs allow comparing repeated runs
// without using question text as an identifier.
type Case struct {
	ID               string           `json:"id"`
	Category         string           `json:"category"`
	Question         string           `json:"question"`
	ExpectedSources  []ExpectedSource `json:"expected_sources"`
	TopicGroups      []TopicGroup     `json:"topic_groups,omitempty"`
	ForbiddenSources []string         `json:"forbidden_sources,omitempty"`
	ExpectedEmpty    bool             `json:"expected_empty,omitempty"`
	RelevanceGrades  map[string]int   `json:"relevance_grades,omitempty"` // fixture_id -> grade (0-3)
}

// ExpectedSource identifies an acceptable document by stable fixture ID.
type ExpectedSource struct {
	FixtureID string `json:"fixture_id"`
	Grade     int    `json:"grade,omitempty"` // relevance grade when present
}

// TopicGroup names a set of acceptable sources that together cover one topic
// of a multi-topic question. Every group must have at least one hit for full
// topic coverage.
type TopicGroup struct {
	GroupID    string   `json:"group_id"`
	FixtureIDs []string `json:"fixture_ids"`
}

// CaseSet is a versioned collection of evaluation cases.
type CaseSet struct {
	Version int    `json:"version"`
	SetID   string `json:"set_id"`
	Cases   []Case `json:"cases"`
}

// LoadCaseSet reads and validates a case-set file.
func LoadCaseSet(path string) (*CaseSet, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read case set %s: %w", path, err)
	}
	var cs CaseSet
	if err := json.Unmarshal(raw, &cs); err != nil {
		return nil, fmt.Errorf("parse case set %s: %w", path, err)
	}
	if err := ValidateCaseSet(&cs); err != nil {
		return nil, err
	}
	return &cs, nil
}

// ValidateCaseSet checks structural invariants before any query runs.
func ValidateCaseSet(cs *CaseSet) error {
	if cs.Version != CaseSchemaVersion {
		return fmt.Errorf("case set version %d, expected %d", cs.Version, CaseSchemaVersion)
	}
	if cs.SetID == "" {
		return fmt.Errorf("case set has no set_id")
	}
	if len(cs.Cases) == 0 {
		return fmt.Errorf("case set has no cases")
	}
	seen := make(map[string]struct{})
	for i, c := range cs.Cases {
		if c.ID == "" {
			return fmt.Errorf("case %d has no id", i)
		}
		if _, dup := seen[c.ID]; dup {
			return fmt.Errorf("duplicate case id %q", c.ID)
		}
		seen[c.ID] = struct{}{}
		if c.Question == "" && !c.ExpectedEmpty {
			return fmt.Errorf("case %q has no question and is not expected-empty", c.ID)
		}
	}
	return nil
}

// FilterCases returns cases matching the given IDs and categories. Empty
// filters return all cases.
func FilterCases(cases []Case, ids, categories []string) []Case {
	idSet := toSet(ids)
	catSet := toSet(categories)
	var out []Case
	for _, c := range cases {
		if len(idSet) > 0 {
			if _, ok := idSet[c.ID]; !ok {
				continue
			}
		}
		if len(catSet) > 0 {
			if _, ok := catSet[c.Category]; !ok {
				continue
			}
		}
		out = append(out, c)
	}
	return out
}

func toSet(ss []string) map[string]struct{} {
	if len(ss) == 0 {
		return nil
	}
	m := make(map[string]struct{}, len(ss))
	for _, s := range ss {
		m[s] = struct{}{}
	}
	return m
}

// ---- Report types ----

// Report is the machine-readable output of an evaluation run.
type Report struct {
	RunID         string           `json:"run_id"`
	SetID         string           `json:"set_id"`
	SetVersion    int              `json:"set_version"`
	WorkspaceID   string           `json:"workspace_id"`
	Timestamp     time.Time        `json:"timestamp"`
	CommitSHA     string           `json:"commit_sha"`
	EmbedModel    string           `json:"embed_model"`
	EmbedDim      int              `json:"embed_dim"`
	RerankModel   string           `json:"rerank_model"`
	VecCandidates int              `json:"vec_candidates"`
	FTSCandidates int              `json:"fts_candidates"`
	RRFK          int              `json:"rrf_k"`
	RerankEnabled bool             `json:"rerank_enabled"`
	TopK          int              `json:"top_k"`
	Cases         []CaseReport     `json:"cases"`
	Summary       Summary          `json:"summary"`
	Latency       LatencyReport    `json:"latency"`
	ExactVsHNSW   *HNSWReport      `json:"exact_vs_hnsw,omitempty"`
	Readiness     *ReadinessReport `json:"readiness,omitempty"`
}

// CaseReport is the result for one evaluation case.
type CaseReport struct {
	CaseID             string       `json:"case_id"`
	Category           string       `json:"category"`
	Passed             bool         `json:"passed"`
	ExpectedEmpty      bool         `json:"expected_empty"`
	Warnings           []string     `json:"warnings"`
	PacketCount        int          `json:"packet_count"`
	SourceRecallAtK    float64      `json:"source_recall_at_k"`
	RRFirstAcceptable  float64      `json:"rr_first_acceptable"`
	NDCG               float64      `json:"ndcg"`
	TopicGroupCoverage float64      `json:"topic_group_coverage"`
	TopicGroupsHit     int          `json:"topic_groups_hit"`
	TopicGroupsTotal   int          `json:"topic_groups_total"`
	ForbiddenHits      int          `json:"forbidden_hits"`
	DenseCandidates    int          `json:"dense_candidates"`
	LexicalCandidates  int          `json:"lexical_candidates"`
	FusedCandidates    int          `json:"fused_candidates"`
	RerankedCandidates int          `json:"reranked_candidates"`
	SelectedPackets    int          `json:"selected_packets"`
	BudgetUsed         int          `json:"budget_used"`
	BudgetAllowed      int          `json:"budget_allowed"`
	BudgetTruncated    bool         `json:"budget_truncated"`
	WarningCount       int          `json:"warning_count"`
	Stages             StageMetrics `json:"stages"`
	LatencyMS          float64      `json:"latency_ms"`
	SubQueries         int          `json:"sub_queries"`
	Failures           []string     `json:"failures,omitempty"`
}

// StageMetrics holds per-stage retrieval observations.
type StageMetrics struct {
	Dense    StageObservation `json:"dense"`
	Lexical  StageObservation `json:"lexical"`
	Fused    StageObservation `json:"fused"`
	Reranked StageObservation `json:"reranked"`
	Selected StageObservation `json:"selected"`
}

// StageObservation is one stage's candidate yield and characteristics.
type StageObservation struct {
	CandidateCount int      `json:"candidate_count"`
	DocIDs         []string `json:"doc_ids,omitempty"`
}

// Summary aggregates case-level results.
type Summary struct {
	TotalCases        int     `json:"total_cases"`
	PassedCases       int     `json:"passed_cases"`
	FailedCases       int     `json:"failed_cases"`
	MeanRecall        float64 `json:"mean_recall"`
	MeanMRR           float64 `json:"mean_mrr"`
	MeanNDCG          float64 `json:"mean_ndcg"`
	MeanTopicCov      float64 `json:"mean_topic_coverage"`
	TotalForbidden    int     `json:"total_forbidden_hits"`
	EmptyPassRate     float64 `json:"empty_pass_rate"`
	TotalWarnings     int     `json:"total_warnings"`
	BudgetTruncations int     `json:"budget_truncations"`
	OverallPassed     bool    `json:"overall_passed"`
}

// LatencyReport holds per-stage latency distributions.
type LatencyReport struct {
	EmbedMS   Distribution `json:"embed_ms"`
	DenseMS   Distribution `json:"dense_ms"`
	LexicalMS Distribution `json:"lexical_ms"`
	FusedMS   Distribution `json:"fused_ms"`
	RerankMS  Distribution `json:"rerank_ms"`
	SelectMS  Distribution `json:"select_ms"`
	ExpandMS  Distribution `json:"expand_ms"`
	TotalMS   Distribution `json:"total_ms"`
}

// Distribution is a summary of a set of measurements.
type Distribution struct {
	Min  float64 `json:"min"`
	Max  float64 `json:"max"`
	Mean float64 `json:"mean"`
	P50  float64 `json:"p50"`
	P95  float64 `json:"p95"`
	P99  float64 `json:"p99"`
	N    int     `json:"n"`
}

// HNSWReport compares exact and approximate dense search.
type HNSWReport struct {
	CasesCompared int              `json:"cases_compared"`
	MeanRecall    float64          `json:"mean_approximate_recall"`
	MinRecall     float64          `json:"min_approximate_recall"`
	PerCase       []HNSWCaseReport `json:"per_case"`
	EfSearch      int              `json:"ef_search"`
	CorpusSize    int              `json:"corpus_size"`
}

// HNSWCaseReport is one case's exact-vs-approximate comparison.
type HNSWCaseReport struct {
	CaseID         string   `json:"case_id"`
	ExactCount     int      `json:"exact_count"`
	HNSWCount      int      `json:"hnsw_count"`
	ExactDocIDs    []string `json:"exact_doc_ids"`
	HNSWDocIDs     []string `json:"hnsw_doc_ids"`
	ApproxRecall   float64  `json:"approximate_recall"`
	HNSWLatencyMS  float64  `json:"hnsw_latency_ms"`
	ExactLatencyMS float64  `json:"exact_latency_ms"`
}

// ThresholdsConfig defines mandatory pass/fail thresholds for synthetic cases.
type ThresholdsConfig struct {
	MinRecallAtK     float64 `json:"min_recall_at_k"`
	MinMRR           float64 `json:"min_mrr"`
	MinNDCG          float64 `json:"min_ndcg"`
	MinTopicCoverage float64 `json:"min_topic_coverage"`
	MaxForbiddenHits int     `json:"max_forbidden_hits"`
	MinEmptyPassRate float64 `json:"min_empty_pass_rate"`
	MinApproxRecall  float64 `json:"min_approximate_recall"`
}

// DefaultSyntheticThresholds returns the mandatory thresholds for the
// synthetic evaluation suite.
func DefaultSyntheticThresholds() ThresholdsConfig {
	return ThresholdsConfig{
		MinRecallAtK:     0.7,
		MinMRR:           0.5,
		MinNDCG:          0.5,
		MinTopicCoverage: 0.75,
		MaxForbiddenHits: 0,
		MinEmptyPassRate: 1.0,
		MinApproxRecall:  0.85,
	}
}

// EvaluateConfig holds all configuration for one evaluation run.
type EvaluateConfig struct {
	WorkspaceID string
	CaseSetPath string
	ReportPath  string
	CommitSHA   string
	FilterIDs   []string
	FilterCats  []string
	JSON        bool
	RunHNSW     bool
	EfSearch    int
	Thresholds  *ThresholdsConfig
}
