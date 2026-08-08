package eval

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os/exec"
	"strings"
	"time"

	"github.com/suankan/pocket-advisor/internal/client/embedding"
	"github.com/suankan/pocket-advisor/internal/client/llm"
	"github.com/suankan/pocket-advisor/internal/client/reranking"
	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/retrieval"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
)

// Evaluator runs evaluation cases against a workspace's retrieval path.
type Evaluator struct {
	DB       *postgres.DB
	Embedder *embedding.Client
	Reranker *reranking.Client
	LLM      *llm.Client
	Config   config.Query
	Log      *slog.Logger
}

// NewEvaluator creates an evaluator from infrastructure components.
func NewEvaluator(
	db *postgres.DB,
	emb *embedding.Client,
	rr *reranking.Client,
	l *llm.Client,
	cfg config.Query,
	log *slog.Logger,
) *Evaluator {
	return &Evaluator{
		DB: db, Embedder: emb, Reranker: rr, LLM: l,
		Config: cfg, Log: log,
	}
}

// Run executes the full evaluation pipeline and returns a report.
func (e *Evaluator) Run(ctx context.Context, evalCfg EvaluateConfig) (*Report, error) {
	cs, err := LoadCaseSet(evalCfg.CaseSetPath)
	if err != nil {
		return nil, err
	}

	cases := FilterCases(cs.Cases, evalCfg.FilterIDs, evalCfg.FilterCats)
	if len(cases) == 0 {
		return nil, fmt.Errorf("no cases match the given filters")
	}

	commitSHA := evalCfg.CommitSHA
	if commitSHA == "" {
		commitSHA = gitSHA()
	}

	report := &Report{
		RunID:         fmt.Sprintf("eval-%s", time.Now().UTC().Format("20060102T150405Z")),
		SetID:         cs.SetID,
		SetVersion:    cs.Version,
		WorkspaceID:   evalCfg.WorkspaceID,
		Timestamp:     time.Now().UTC(),
		CommitSHA:     commitSHA,
		EmbedModel:    e.Embedder.Model(),
		RerankModel:   e.Reranker.Model(),
		VecCandidates: e.Config.VecCandidates,
		FTSCandidates: e.Config.FTSCandidates,
		RRFK:          e.Config.RRFK,
		RerankEnabled: e.Config.RerankEnabled,
		TopK:          e.Config.DefaultTopK,
	}

	// Build the retrieval service for this workspace.
	svc := retrieval.New(e.DB, e.Embedder, e.Reranker, e.LLM,
		e.Config, evalCfg.WorkspaceID, e.Log)

	if err := svc.AssertScope(ctx); err != nil {
		return nil, fmt.Errorf("assert scope: %w", err)
	}

	// Build fixture lookup from the database.
	lookup, err := e.buildFixtureLookup(ctx)
	if err != nil {
		return nil, fmt.Errorf("build fixture lookup: %w", err)
	}

	// Run each case.
	var totalEmbed, totalDense, totalLexical, totalFuse, totalRerank, totalSelect, totalExpand, totalTotal []float64
	var allFailures []string

	for _, c := range cases {
		cr, err := e.runCase(ctx, svc, c, lookup)
		if err != nil {
			e.Log.Warn("case failed", "id", c.ID, "error", err)
			cr = &CaseReport{
				CaseID:   c.ID,
				Category: c.Category,
				Passed:   false,
				Failures: []string{fmt.Sprintf("execution error: %v", err)},
			}
		}
		report.Cases = append(report.Cases, *cr)

		if !cr.Passed {
			for _, f := range cr.Failures {
				allFailures = append(allFailures, fmt.Sprintf("%s: %s", c.ID, f))
			}
		}

		// Accumulate latencies.
		totalTotal = append(totalTotal, cr.LatencyMS)
	}

	// Build summary.
	report.Summary = e.buildSummary(report.Cases, evalCfg)

	// Build latency distributions.
	report.Latency = LatencyReport{
		TotalMS:   DistributionFrom(totalTotal),
		EmbedMS:   DistributionFrom(totalEmbed),
		DenseMS:   DistributionFrom(totalDense),
		LexicalMS: DistributionFrom(totalLexical),
		FusedMS:   DistributionFrom(totalFuse),
		RerankMS:  DistributionFrom(totalRerank),
		SelectMS:  DistributionFrom(totalSelect),
		ExpandMS:  DistributionFrom(totalExpand),
	}

	// Exact vs HNSW comparison.
	if evalCfg.RunHNSW {
		hnswReport, err := e.runHNSWComparison(ctx, svc, cases, evalCfg)
		if err != nil {
			e.Log.Warn("HNSW comparison failed", "error", err)
		} else {
			report.ExactVsHNSW = hnswReport
		}
	}

	return report, nil
}

// runCase evaluates a single case.
func (e *Evaluator) runCase(ctx context.Context, svc *retrieval.Service, c Case, lookup *FixtureLookup) (*CaseReport, error) {
	cr := &CaseReport{
		CaseID:        c.ID,
		Category:      c.Category,
		ExpectedEmpty: c.ExpectedEmpty,
	}

	req := retrieval.Request{
		Question: c.Question,
		TopK:     e.Config.DefaultTopK,
	}

	start := time.Now()

	// Use the stage-level evaluation path.
	sr, err := svc.Evaluate(ctx, req)
	if err != nil {
		return nil, err
	}

	cr.LatencyMS = float64(time.Since(start).Milliseconds())
	cr.Warnings = sr.Warnings
	cr.WarningCount = len(sr.Warnings)
	cr.BudgetUsed = sr.Budget.BytesUsed
	cr.BudgetAllowed = sr.Budget.BytesAllowed
	cr.SubQueries = len(sr.SubQueries)

	// Budget truncation.
	for _, w := range sr.Warnings {
		if w == retrieval.WarnBudgetTruncated {
			cr.BudgetTruncated = true
			break
		}
	}

	// Convert selected packets to packetRefs with fixture ID resolution.
	var packets []packetRef
	for _, p := range sr.SelectedPackets {
		packets = append(packets, packetRef{
			DocID:     p.DocID,
			FixtureID: lookup.Resolve(p.DocID, p.SHA256),
			Score:     p.Match.Score,
		})
	}
	cr.PacketCount = len(packets)
	cr.SelectedPackets = len(packets)

	// Stage metrics.
	cr.Stages = StageMetrics{
		Dense: StageObservation{
			CandidateCount: sr.DenseCount,
		},
		Lexical: StageObservation{
			CandidateCount: sr.LexicalCount,
		},
		Fused: StageObservation{
			CandidateCount: sr.FusedCount,
			DocIDs:         sr.FusedDocIDs(),
		},
		Reranked: StageObservation{
			CandidateCount: sr.RerankCount,
		},
		Selected: StageObservation{
			CandidateCount: sr.SelectCount,
			DocIDs:         UniqueDocIDs(packets),
		},
	}
	cr.DenseCandidates = sr.DenseCount
	cr.LexicalCandidates = sr.LexicalCount
	cr.FusedCandidates = sr.FusedCount
	cr.RerankedCandidates = sr.RerankCount

	// Build expected source set from the case.
	var expected []ExpectedSource
	for _, es := range c.ExpectedSources {
		expected = append(expected, ExpectedSource{
			FixtureID: es.FixtureID,
			Grade:     es.Grade,
		})
	}

	// Metrics.
	cr.SourceRecallAtK = SourceRecallAtK(packets, expected, e.Config.DefaultTopK)
	cr.RRFirstAcceptable = ReciprocalRankFirst(packets, expected)
	cr.NDCG = NDCG(packets, expected, c.RelevanceGrades)

	// Topic group coverage.
	if len(c.TopicGroups) > 0 {
		cr.TopicGroupCoverage = TopicGroupCoverage(packets, c.TopicGroups)
		cr.TopicGroupsTotal = len(c.TopicGroups)
		cr.TopicGroupsHit = int(cr.TopicGroupCoverage * float64(cr.TopicGroupsTotal))
	}

	// Forbidden hits.
	cr.ForbiddenHits = ForbiddenHits(packets, c.ForbiddenSources)

	// Determine pass/fail.
	cr.Passed = true
	cr.Failures = e.checkCase(c, cr)

	if len(cr.Failures) > 0 {
		cr.Passed = false
	}

	return cr, nil
}

// checkCase validates a case report against expected outcomes.
func (e *Evaluator) checkCase(c Case, cr *CaseReport) []string {
	var failures []string

	if c.ExpectedEmpty {
		if cr.PacketCount > 0 {
			failures = append(failures, fmt.Sprintf(
				"expected empty result, got %d packets", cr.PacketCount))
		}
		return failures
	}

	if len(c.ExpectedSources) > 0 && cr.PacketCount == 0 {
		failures = append(failures, "expected sources but got zero packets")
	}

	if cr.ForbiddenHits > 0 {
		failures = append(failures, fmt.Sprintf(
			"%d forbidden source(s) in results", cr.ForbiddenHits))
	}

	return failures
}

// buildSummary aggregates case reports.
func (e *Evaluator) buildSummary(cases []CaseReport, cfg EvaluateConfig) Summary {
	s := Summary{
		TotalCases: len(cases),
	}

	var recallSum, mrrSum, ndcgSum, topicCovSum float64
	var emptyCases, emptyPassed int
	var emptyExpected int

	thresholds := DefaultSyntheticThresholds()
	if cfg.Thresholds != nil {
		thresholds = *cfg.Thresholds
	}

	for _, c := range cases {
		if c.Passed {
			s.PassedCases++
		} else {
			s.FailedCases++
		}

		if c.ExpectedEmpty {
			emptyCases++
			emptyExpected++
			if c.PacketCount == 0 {
				emptyPassed++
			}
		}

		recallSum += c.SourceRecallAtK
		mrrSum += c.RRFirstAcceptable
		ndcgSum += c.NDCG
		topicCovSum += c.TopicGroupCoverage

		s.TotalForbidden += c.ForbiddenHits
		s.TotalWarnings += c.WarningCount
		if c.BudgetTruncated {
			s.BudgetTruncations++
		}
	}

	n := float64(len(cases))
	if n > 0 {
		s.MeanRecall = recallSum / n
		s.MeanMRR = mrrSum / n
		s.MeanNDCG = ndcgSum / n
		s.MeanTopicCov = topicCovSum / n
	}

	if emptyExpected > 0 {
		s.EmptyPassRate = float64(emptyPassed) / float64(emptyExpected)
	}

	// Evaluate thresholds.
	s.OverallPassed = true
	if s.MeanRecall < thresholds.MinRecallAtK {
		s.OverallPassed = false
	}
	if s.MeanMRR < thresholds.MinMRR {
		s.OverallPassed = false
	}
	if s.MeanNDCG < thresholds.MinNDCG {
		s.OverallPassed = false
	}
	if s.TotalForbidden > thresholds.MaxForbiddenHits {
		s.OverallPassed = false
	}
	if s.EmptyPassRate < thresholds.MinEmptyPassRate && emptyExpected > 0 {
		s.OverallPassed = false
	}

	return s
}

// buildFixtureLookup reads document metadata to build a doc_id -> fixture_id
// mapping. For synthetic cases, fixture IDs are stored in the document's
// source_filename or metadata.
func (e *Evaluator) buildFixtureLookup(ctx context.Context) (*FixtureLookup, error) {
	byDocID := make(map[string]string)
	byHash := make(map[string]string)

	rows, err := e.DB.Pool.Query(ctx, `
SELECT doc_id::text, raw_sha256,
       COALESCE(NULLIF(source_filename, ''), '')
FROM documents`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var docID, sha, filename string
		if err := rows.Scan(&docID, &sha, &filename); err != nil {
			return nil, err
		}
		// Synthetic fixtures use a stable naming convention: the filename
		// without extension is the fixture ID.
		if filename != "" {
			fid := stripExtension(filename)
			byDocID[docID] = fid
			if sha != "" {
				byHash[sha] = fid
			}
		}
	}
	return NewFixtureLookup(byDocID, byHash), rows.Err()
}

func stripExtension(name string) string {
	if i := strings.LastIndex(name, "."); i > 0 {
		return name[:i]
	}
	return name
}

func gitSHA() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// runHNSWComparison compares exact and HNSW dense search for every case.
func (e *Evaluator) runHNSWComparison(
	ctx context.Context,
	svc *retrieval.Service,
	cases []Case,
	cfg EvaluateConfig,
) (*HNSWReport, error) {
	efSearch := cfg.EfSearch
	if efSearch <= 0 {
		efSearch = 40
	}

	corpusSize, err := svc.CorpusSize(ctx)
	if err != nil {
		return nil, fmt.Errorf("corpus size: %w", err)
	}

	var perCase []HNSWCaseReport
	var recalls []float64

	for _, c := range cases {
		if c.ExpectedEmpty || len(c.ExpectedSources) == 0 {
			continue
		}

		// Embed the question.
		vecs, err := e.Embedder.Embed(ctx, []string{c.Question})
		if err != nil {
			e.Log.Warn("embed for HNSW comparison failed", "case", c.ID, "error", err)
			continue
		}

		exactCands, exactMs, err := svc.EvaluateExactSearch(ctx, vecs[0], e.Embedder.Model(), 50)
		if err != nil {
			e.Log.Warn("exact search failed", "case", c.ID, "error", err)
			continue
		}

		hnswCands, hnswMs, err := svc.EvaluateHNSWSearch(ctx, vecs[0], e.Embedder.Model(), 50, efSearch)
		if err != nil {
			e.Log.Warn("HNSW search failed", "case", c.ID, "error", err)
			continue
		}

		// Compute approximate recall: fraction of exact top-k found by HNSW.
		exactDocs := docSet(exactCands)
		hnswDocs := docSet(hnswCands)
		recall := approximateRecall(exactDocs, hnswDocs)
		recalls = append(recalls, recall)

		perCase = append(perCase, HNSWCaseReport{
			CaseID:         c.ID,
			ExactCount:     len(exactCands),
			HNSWCount:      len(hnswCands),
			ExactDocIDs:    exactDocs,
			HNSWDocIDs:     hnswDocs,
			ApproxRecall:   recall,
			HNSWLatencyMS:  hnswMs,
			ExactLatencyMS: exactMs,
		})
	}

	var meanRecall, minRecall float64
	if len(recalls) > 0 {
		sum := 0.0
		minRecall = math.MaxFloat64
		for _, r := range recalls {
			sum += r
			if r < minRecall {
				minRecall = r
			}
		}
		meanRecall = sum / float64(len(recalls))
	}

	return &HNSWReport{
		CasesCompared: len(perCase),
		MeanRecall:    meanRecall,
		MinRecall:     minRecall,
		PerCase:       perCase,
		EfSearch:      efSearch,
		CorpusSize:    corpusSize,
	}, nil
}

func docSet(cands []retrieval.EvalCandidate) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, c := range cands {
		if _, dup := seen[c.DocID]; dup {
			continue
		}
		seen[c.DocID] = struct{}{}
		out = append(out, c.DocID)
	}
	return out
}

func approximateRecall(exact, approximate []string) float64 {
	if len(exact) == 0 {
		return 1.0
	}
	approxSet := make(map[string]struct{}, len(approximate))
	for _, d := range approximate {
		approxSet[d] = struct{}{}
	}
	hit := 0
	for _, d := range exact {
		if _, ok := approxSet[d]; ok {
			hit++
		}
	}
	return float64(hit) / float64(len(exact))
}
