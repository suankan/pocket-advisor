// Package eval provides a transport-independent retrieval evaluation workflow.
//
// It measures dense, lexical, fused, reranked, and selected retrieval stages
// against versioned evaluation cases. The CLI and MCP adapters are thin
// boundaries over this package (retrieval-design.md section 7).
package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// CaseSchemaVersion is the current evaluation case format version.
const CaseSchemaVersion = 4

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
	ID                          string             `json:"id"`
	Category                    string             `json:"category"`
	Question                    string             `json:"question"`
	ExpectedDocuments           []ExpectedDocument `json:"expected_documents"`
	ExpectedFacts               []ExpectedFact     `json:"expected_facts,omitempty"`
	RequireAllExpectedDocuments bool               `json:"require_all_expected_documents,omitempty"`
	ExpectedEmpty               bool               `json:"expected_empty,omitempty"`
}

// DocumentMatch names a document by its content rather than by its row
// identity, and is resolved against the evaluated workspace before any query
// runs.
//
// doc_id was the obvious key and turned out to be the wrong one. It is a
// random UUID minted per database, so rebuilding a workspace from the same
// original files renumbers every document and silently invalidates a whole
// curated suite: every case reports "no expected document in results" while
// retrieval is working perfectly, which reads as a quality collapse rather
// than as the setup error it is. raw_sha256 survives that, because it hashes
// the original bytes and is the same key CreateStub already upserts on, so
// "the same bytes are the same document" is enforced by the schema rather
// than assumed here.
//
// The trade is deliberate rather than free. doc_id survives a document's
// bytes changing and nothing else; raw_sha256 survives everything except
// that. For an archived corpus, re-ingestion is routine and re-export is
// rare, so the second failure is the one worth accepting.
type DocumentMatch struct {
	RawSHA256 string `json:"raw_sha256"`
}

// ExpectedDocument is a document a correct answer may rest on, with the
// relevance grade nDCG scores it against.
type ExpectedDocument struct {
	Match DocumentMatch `json:"match"`
	Grade int           `json:"grade"`
}

// ExpectedFact is text a correct answer must rest on, checked against the
// evidence a run actually returned rather than against what it merely ranked.
//
// It exists because document recall cannot see the failure that matters most
// often in practice: the right document is retrieved, and its text is then
// dropped by the shared context budget. Recall scores 1.0, the case passes,
// and the answer is nowhere in the response. A fact assertion fails that
// correctly.
//
// Matching collapses whitespace runs on both sides, using the same rule that
// defines shared passage identity, so PDF layout spacing does not decide the
// outcome. CaseSensitive is for the rare question about styling rather than
// content, where "iStem" and "iSTEM" are different answers.
type ExpectedFact struct {
	Span          string `json:"span"`
	Grade         int    `json:"grade"`
	CaseSensitive bool   `json:"case_sensitive,omitempty"`
}

// CaseSet is a versioned collection of evaluation cases.
type CaseSet struct {
	Version int    `json:"version"`
	SetID   string `json:"set_id"`
	Cases   []Case `json:"cases"`
}

// CaseSetSHA256 returns a canonical content identity for a case set. Reports
// use it to prevent measurements from different curated suites being
// compared as though they exercised the same questions.
func CaseSetSHA256(cs *CaseSet) string {
	canonical, err := json.Marshal(cs)
	if err != nil {
		panic(fmt.Sprintf("marshal case set for digest: %v", err))
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
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
		if err := validateCaseDocumentIDs(c); err != nil {
			return fmt.Errorf("case %q: %w", c.ID, err)
		}
	}
	return nil
}

func validateCaseDocumentIDs(c Case) error {
	if c.ExpectedEmpty {
		if len(c.ExpectedDocuments) > 0 {
			return fmt.Errorf("expected-empty case has expected documents")
		}
		if c.RequireAllExpectedDocuments {
			return fmt.Errorf("expected-empty case requires all expected documents")
		}
	} else if len(c.ExpectedDocuments) == 0 {
		return fmt.Errorf("non-empty case has no expected documents")
	}

	expected := make(map[string]struct{}, len(c.ExpectedDocuments))
	for i, document := range c.ExpectedDocuments {
		if !isSHA256Hex(document.Match.RawSHA256) {
			return fmt.Errorf("expected document %d has invalid raw_sha256", i+1)
		}
		if _, duplicate := expected[document.Match.RawSHA256]; duplicate {
			return fmt.Errorf("expected document %d repeats an earlier raw_sha256", i+1)
		}
		expected[document.Match.RawSHA256] = struct{}{}
		if document.Grade < 0 || document.Grade > 3 {
			return fmt.Errorf("expected document %d has grade %d outside 0-3", i+1, document.Grade)
		}
	}

	// Spans are the answer, so an empty one would pass against any evidence at
	// all. Reject it here rather than let it score a silent free pass.
	for i, fact := range c.ExpectedFacts {
		if strings.TrimSpace(fact.Span) == "" {
			return fmt.Errorf("expected fact %d has an empty span", i+1)
		}
		if fact.Grade < 0 || fact.Grade > 3 {
			return fmt.Errorf("expected fact %d has grade %d outside 0-3", i+1, fact.Grade)
		}
	}
	if c.ExpectedEmpty && len(c.ExpectedFacts) > 0 {
		return fmt.Errorf("expected-empty case asserts facts")
	}
	return nil
}

// isSHA256Hex accepts only the lowercase form the documents table stores, so
// a case set and a database row compare as plain strings everywhere else.
func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
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
	RunID         string            `json:"run_id"`
	SetID         string            `json:"set_id"`
	SetVersion    int               `json:"set_version"`
	CaseSetSHA256 string            `json:"case_set_sha256"`
	WorkspaceID   string            `json:"workspace_id"`
	Timestamp     time.Time         `json:"timestamp"`
	CommitSHA     string            `json:"commit_sha"`
	EmbedModel    string            `json:"embed_model"`
	EmbedDim      int               `json:"embed_dim"`
	RerankModel   string            `json:"rerank_model"`
	VecCandidates int               `json:"vec_candidates"`
	FTSCandidates int               `json:"fts_candidates"`
	RRFK          int               `json:"rrf_k"`
	RerankEnabled bool              `json:"rerank_enabled"`
	TopK          int               `json:"top_k"`
	Cases         []CaseReport      `json:"cases"`
	Summary       Summary           `json:"summary"`
	Latency       LatencyReport     `json:"latency"`
	ExactVsHNSW   *HNSWReport       `json:"exact_vs_hnsw,omitempty"`
	Readiness     *ReadinessReport  `json:"readiness,omitempty"`
	TopicGraph    *TopicGraphReport `json:"topic_graph,omitempty"`
}

// CaseReport is the result for one evaluation case.
type CaseReport struct {
	CaseID                  string   `json:"case_id"`
	Category                string   `json:"category"`
	Passed                  bool     `json:"passed"`
	ExpectedEmpty           bool     `json:"expected_empty"`
	ExpectedDocumentCount   int      `json:"expected_document_count"`
	ExpectedFactCount       int      `json:"expected_fact_count"`
	Warnings                []string `json:"warnings"`
	PacketCount             int      `json:"packet_count"`
	DocumentRecallAtK       float64  `json:"document_recall_at_k"`
	RRFirstExpectedDocument float64  `json:"rr_first_expected_document"`
	NDCG                    float64  `json:"ndcg"`
	FactRecall              float64  `json:"fact_recall"`
	RRFirstFact             float64  `json:"rr_first_fact"`
	// AnswerRank is the 1-based position of the first returned packet whose
	// admitted text grounds an asserted fact, and 0 when none does. It is the
	// same observation RRFirstFact carries, in the form a person reads a
	// single run with: reciprocal rank averages well across a suite, a raw
	// position says how far down the answer actually was. Neither gates a
	// case — depth is watched as a trend, not asserted per case.
	AnswerRank int `json:"answer_rank"`
	// ExpectedDocumentRank is the same for the first expected document, and is
	// the diagnostic that separates the two ways depth goes wrong: a good
	// document rank with no answer rank means the budget dropped the text,
	// while a poor document rank means the ranker did not find it.
	ExpectedDocumentRank int          `json:"expected_document_rank"`
	DenseCandidates      int          `json:"dense_candidates"`
	LexicalCandidates    int          `json:"lexical_candidates"`
	FusedCandidates      int          `json:"fused_candidates"`
	RerankedCandidates   int          `json:"reranked_candidates"`
	SelectedPackets      int          `json:"selected_packets"`
	BudgetUsed           int          `json:"budget_used"`
	BudgetAllowed        int          `json:"budget_allowed"`
	BudgetTruncated      bool         `json:"budget_truncated"`
	WarningCount         int          `json:"warning_count"`
	Stages               StageMetrics `json:"stages"`
	LatencyMS            float64      `json:"latency_ms"`
	SubQueries           int          `json:"sub_queries"`
	Failures             []string     `json:"failures,omitempty"`
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
	RankedCases       int     `json:"ranked_cases"`
	MeanRecall        float64 `json:"mean_recall"`
	MeanMRR           float64 `json:"mean_mrr"`
	MeanNDCG          float64 `json:"mean_ndcg"`
	MeanFactRecall    float64 `json:"mean_fact_recall"`
	MeanFactMRR       float64 `json:"mean_fact_mrr"`
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

// ThresholdsConfig defines mandatory pass/fail thresholds for curated cases.
type ThresholdsConfig struct {
	MinRecallAtK     float64 `json:"min_recall_at_k"`
	MinMRR           float64 `json:"min_mrr"`
	MinNDCG          float64 `json:"min_ndcg"`
	MinEmptyPassRate float64 `json:"min_empty_pass_rate"`
	MinApproxRecall  float64 `json:"min_approximate_recall"`
	// TopicGraph is opt-in. When Mandatory is true, failed graph gates make
	// --evaluate return non-zero; otherwise its graph measurements are informational.
	TopicGraph *TopicGraphThresholds `json:"topic_graph,omitempty"`
}

// DefaultThresholds returns the mandatory thresholds for the
// curated evaluation suite.
func DefaultThresholds() ThresholdsConfig {
	return ThresholdsConfig{
		MinRecallAtK:     0.7,
		MinMRR:           0.5,
		MinNDCG:          0.5,
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
