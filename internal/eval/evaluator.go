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
	DB              *postgres.DB
	Embedder        *embedding.Client
	Reranker        *reranking.Client
	LLM             *llm.Client
	Config          config.Query
	Log             *slog.Logger
	TopicGraphStore TopicGraphEvaluationStore
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
		Config: cfg, Log: log, TopicGraphStore: postgres.NewTopicGraphEvaluationStore(db),
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

	// Resolve every document reference before the first query. An unusable
	// suite has to fail here, loudly and as a setup error, rather than run to
	// completion and report a quality verdict its own preconditions did not
	// support.
	resolved, err := ResolveCases(ctx, postgres.NewDocumentRepo(e.DB), cases)
	if err != nil {
		return nil, err
	}

	commitSHA := evalCfg.CommitSHA
	if commitSHA == "" {
		commitSHA = gitSHA()
	}

	report := &Report{
		RunID:         fmt.Sprintf("eval-%s", time.Now().UTC().Format("20060102T150405Z")),
		SetID:         cs.SetID,
		SetVersion:    cs.Version,
		CaseSetSHA256: CaseSetSHA256(cs),
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

	// Run each case.
	var stageResults []*retrieval.StageResult
	var allFailures []string

	for _, c := range resolved {
		cr, sr, err := e.runCase(ctx, svc, c)
		if err != nil {
			e.Log.Warn("case failed", "id", c.ID, "error", err)
			cr = &CaseReport{
				CaseID:                c.ID,
				Category:              c.Category,
				ExpectedEmpty:         c.ExpectedEmpty,
				ExpectedDocumentCount: len(c.Expected),
				ExpectedFactCount:     len(c.ExpectedFacts),
				Passed:                false,
				Failures:              []string{fmt.Sprintf("execution error: %v", err)},
			}
		} else {
			stageResults = append(stageResults, sr)
		}
		report.Cases = append(report.Cases, *cr)

		if !cr.Passed {
			for _, f := range cr.Failures {
				allFailures = append(allFailures, fmt.Sprintf("%s: %s", c.ID, f))
			}
		}
	}

	// Build summary.
	report.Summary = e.buildSummary(report.Cases, evalCfg)

	// Build latency distributions from successful stage-level evaluations.
	report.Latency = latencyReport(stageResults)

	// Exact vs HNSW comparison. Its recall threshold applies only when this
	// optional comparison produced at least one comparable case.
	if evalCfg.RunHNSW {
		hnswReport, err := e.runHNSWComparison(ctx, svc, cases, evalCfg)
		if err != nil {
			e.Log.Warn("HNSW comparison failed", "error", err)
		} else {
			report.ExactVsHNSW = hnswReport
			applyHNSWThreshold(&report.Summary, hnswReport, thresholdsFor(evalCfg))
		}
	}

	// A graph is evaluated only when the workspace has an active version. The
	// private store keeps all graph identifiers inside this call; the report is
	// aggregate-only. A missing graph is still represented when graph gates are
	// configured, so a mandatory active-graph requirement cannot be bypassed.
	if e.TopicGraphStore != nil {
		graph, err := EvaluateTopicGraph(ctx, e.TopicGraphStore)
		if err != nil {
			return nil, fmt.Errorf("evaluate topic graph: %w", err)
		}
		if graph.ActiveVersion || thresholdsFor(evalCfg).TopicGraph != nil {
			report.TopicGraph = graph
			applyTopicGraphThresholds(&report.Summary, graph, thresholdsFor(evalCfg))
		}
	}

	return report, nil
}

func latencyReport(stageResults []*retrieval.StageResult) LatencyReport {
	var embed, dense, lexical, fused, rerank, selectStage, expand, total []float64
	for _, sr := range stageResults {
		embed = append(embed, milliseconds(sr.EmbedDuration))
		// Dense and lexical candidate generation share the fused SQL round trip.
		// Evaluate records that overlapping wall-clock observation for each leg;
		// if a caller supplies no observation, preserve an empty distribution
		// rather than reporting a measured zero duration.
		if sr.DenseDuration > 0 {
			dense = append(dense, milliseconds(sr.DenseDuration))
		}
		if sr.LexicalDuration > 0 {
			lexical = append(lexical, milliseconds(sr.LexicalDuration))
		}
		fused = append(fused, milliseconds(sr.FuseDuration))
		rerank = append(rerank, milliseconds(sr.RerankDuration))
		selectStage = append(selectStage, milliseconds(sr.SelectDuration))
		expand = append(expand, milliseconds(sr.ExpandDuration))
		total = append(total, milliseconds(sr.TotalDuration))
	}

	return LatencyReport{
		EmbedMS:   DistributionFrom(embed),
		DenseMS:   DistributionFrom(dense),
		LexicalMS: DistributionFrom(lexical),
		FusedMS:   DistributionFrom(fused),
		RerankMS:  DistributionFrom(rerank),
		SelectMS:  DistributionFrom(selectStage),
		ExpandMS:  DistributionFrom(expand),
		TotalMS:   DistributionFrom(total),
	}
}

func milliseconds(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// runCase evaluates a single case.
func (e *Evaluator) runCase(ctx context.Context, svc *retrieval.Service, c ResolvedCase) (*CaseReport, *retrieval.StageResult, error) {
	cr := &CaseReport{
		CaseID:                c.ID,
		Category:              c.Category,
		ExpectedEmpty:         c.ExpectedEmpty,
		ExpectedDocumentCount: len(c.Expected),
		ExpectedFactCount:     len(c.ExpectedFacts),
	}

	req := retrieval.Request{
		Question: c.Question,
		TopK:     e.Config.DefaultTopK,
	}

	// Use the stage-level evaluation path.
	sr, err := svc.Evaluate(ctx, req)
	if err != nil {
		return nil, nil, err
	}

	cr.LatencyMS = milliseconds(sr.TotalDuration)
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

	// Packets carry both the document id metrics compare and the text the run
	// actually admitted. Related-document text counts as admitted evidence
	// too: it reaches the caller in the same response, so a fact grounded
	// there is genuinely groundable.
	var packets []packetRef
	for _, p := range sr.Packets {
		text := p.Text
		for _, related := range p.Related {
			if related.Text != "" {
				text += "\n" + related.Text
			}
		}
		packets = append(packets, packetRef{
			DocID: p.DocID,
			Score: p.Match.Score,
			Text:  text,
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

	// Document metrics compare resolved UUIDs; fact metrics compare admitted
	// text. They disagree exactly when a document was ranked but its text was
	// not returned, which is the case worth seeing.
	cr.DocumentRecallAtK = DocumentRecallAtK(packets, c.Expected, e.Config.DefaultTopK)
	cr.RRFirstExpectedDocument = ReciprocalRankFirstExpectedDocument(packets, c.Expected)
	cr.NDCG = NDCG(packets, c.Expected)
	cr.FactRecall = FactRecall(packets, c.ExpectedFacts)
	cr.RRFirstFact = ReciprocalRankFirstFact(packets, c.ExpectedFacts)
	cr.AnswerRank = FirstFactRank(packets, c.ExpectedFacts)
	cr.ExpectedDocumentRank = FirstExpectedDocumentRank(packets, c.Expected)

	// Determine pass/fail.
	cr.Passed = true
	cr.Failures = e.checkCase(c, cr)

	if len(cr.Failures) > 0 {
		cr.Passed = false
	}

	return cr, sr, nil
}

// checkCase validates a case report against the case's explicit contracts.
// Ranking-quality thresholds remain aggregate gates; this check deliberately
// uses no numeric quality threshold.
func (e *Evaluator) checkCase(c ResolvedCase, cr *CaseReport) []string {
	var failures []string

	if c.ExpectedEmpty && cr.PacketCount > 0 {
		failures = append(failures, fmt.Sprintf(
			"expected empty result, got %d packets", cr.PacketCount))
	}

	// ExpectedDocuments is an acceptable-document set. A regular case needs at
	// least one expected document in the results, or every expected document
	// when its contract explicitly requires complete coverage.
	if !c.ExpectedEmpty {
		if c.RequireAllExpectedDocuments {
			if cr.DocumentRecallAtK < 1 {
				failures = append(failures, fmt.Sprintf(
					"found %d of %d expected documents", int(math.Round(cr.DocumentRecallAtK*float64(cr.ExpectedDocumentCount))), cr.ExpectedDocumentCount))
			}
		} else if cr.DocumentRecallAtK == 0 {
			failures = append(failures, "no expected document in results")
		}
	}

	// Every asserted fact must be present in the admitted evidence. A partial
	// answer is not a correct one, and the case exists to prove the evidence
	// would support the whole thing.
	if len(c.ExpectedFacts) > 0 && cr.FactRecall < 1 {
		grounded := int(math.Round(cr.FactRecall * float64(len(c.ExpectedFacts))))
		failures = append(failures, fmt.Sprintf(
			"evidence grounds %d of %d expected facts", grounded, len(c.ExpectedFacts)))
	}

	return failures
}

func thresholdsFor(cfg EvaluateConfig) ThresholdsConfig {
	if cfg.Thresholds != nil {
		return *cfg.Thresholds
	}
	return DefaultThresholds()
}

// buildSummary aggregates case reports.
func (e *Evaluator) buildSummary(cases []CaseReport, cfg EvaluateConfig) Summary {
	s := Summary{
		TotalCases: len(cases),
	}

	var recallSum, mrrSum, ndcgSum float64
	var factRecallSum, factMRRSum float64
	var factCases int
	var emptyPassed, emptyExpected int
	thresholds := thresholdsFor(cfg)

	for _, c := range cases {
		if c.Passed {
			s.PassedCases++
		} else {
			s.FailedCases++
		}

		if c.ExpectedEmpty {
			emptyExpected++
			if c.PacketCount == 0 {
				emptyPassed++
			}
		}

		// Expected-empty cases have no relevance ranking to score. Their
		// metrics must not be treated as perfect retrieval outcomes.
		if c.ExpectedDocumentCount > 0 {
			s.RankedCases++
			recallSum += c.DocumentRecallAtK
			mrrSum += c.RRFirstExpectedDocument
			ndcgSum += c.NDCG
		}
		// Fact metrics have their own denominator: a case asserting no fact
		// would otherwise contribute a free 1.0 and flatter every run that
		// contained one.
		if c.ExpectedFactCount > 0 {
			factCases++
			factRecallSum += c.FactRecall
			factMRRSum += c.RRFirstFact
		}
		s.TotalWarnings += c.WarningCount
		if c.BudgetTruncated {
			s.BudgetTruncations++
		}
	}

	if s.RankedCases > 0 {
		denominator := float64(s.RankedCases)
		s.MeanRecall = recallSum / denominator
		s.MeanMRR = mrrSum / denominator
		s.MeanNDCG = ndcgSum / denominator
	}
	if factCases > 0 {
		s.MeanFactRecall = factRecallSum / float64(factCases)
		s.MeanFactMRR = factMRRSum / float64(factCases)
	}
	if emptyExpected > 0 {
		s.EmptyPassRate = float64(emptyPassed) / float64(emptyExpected)
	}

	// Evaluate thresholds only for the case types whose metric they measure.
	s.OverallPassed = true
	if s.RankedCases > 0 && s.MeanRecall < thresholds.MinRecallAtK {
		s.OverallPassed = false
	}
	if s.RankedCases > 0 && s.MeanMRR < thresholds.MinMRR {
		s.OverallPassed = false
	}
	if s.RankedCases > 0 && s.MeanNDCG < thresholds.MinNDCG {
		s.OverallPassed = false
	}
	if s.EmptyPassRate < thresholds.MinEmptyPassRate && emptyExpected > 0 {
		s.OverallPassed = false
	}

	return s
}

// applyHNSWThreshold applies the approximate-recall gate only to an actual
// exact-vs-HNSW comparison. It is not a retrieval result-quality threshold.
func applyHNSWThreshold(s *Summary, hnsw *HNSWReport, thresholds ThresholdsConfig) {
	if hnsw != nil && hnsw.CasesCompared > 0 && hnsw.MeanRecall < thresholds.MinApproxRecall {
		s.OverallPassed = false
	}
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
		if c.ExpectedEmpty || len(c.ExpectedDocuments) == 0 {
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
