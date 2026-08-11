package retrieval

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// StageResult is the evaluation-only observation of the retrieval pipeline.
// It exposes intermediate stage data without changing the production Result
// contract (retrieval-design.md section 7).
type StageResult struct {
	Question   string
	SubQueries []string
	Warnings   []string
	Budget     Budget

	// Per-stage candidate counts and doc IDs.
	DenseCandidates    []EvalCandidate
	LexicalCandidates  []EvalCandidate
	FusedCandidates    []EvalCandidate
	RerankedCandidates []scored
	SelectedPackets    []Packet

	// Latency per stage.
	EmbedDuration   time.Duration
	DenseDuration   time.Duration
	LexicalDuration time.Duration
	FuseDuration    time.Duration
	RerankDuration  time.Duration
	SelectDuration  time.Duration
	ExpandDuration  time.Duration
	TotalDuration   time.Duration

	// Exact search results for HNSW comparison.
	ExactCandidates []EvalCandidate

	// Candidate counts at each stage.
	DenseCount   int
	LexicalCount int
	FusedCount   int
	RerankCount  int
	SelectCount  int
}

// FusedDocIDs returns unique doc IDs from fused candidates.
func (sr *StageResult) FusedDocIDs() []string {
	seen := make(map[string]struct{})
	var out []string
	for _, c := range sr.FusedCandidates {
		if _, dup := seen[c.DocID]; dup {
			continue
		}
		seen[c.DocID] = struct{}{}
		out = append(out, c.DocID)
	}
	return out
}

// EvalCandidate is the public type for evaluation stage observations.
type EvalCandidate struct {
	ChunkID   string
	DocID     string
	DenseRank int
	LexRank   int
	RRF       float64
}

// Evaluate runs the full retrieval pipeline and returns stage-level
// observations. This is an evaluation-only method that does not change the
// production Query contract.
func (s *Service) Evaluate(ctx context.Context, req Request) (*StageResult, error) {
	question := strings.TrimSpace(req.Question)
	if question == "" {
		return nil, fmt.Errorf("empty question")
	}
	topK := req.TopK
	if topK <= 0 {
		topK = s.cfg.DefaultTopK
	}

	cfg := s.cfg
	if req.Rerank != nil {
		cfg.RerankEnabled = *req.Rerank
	}
	if req.Decompose != nil {
		cfg.DecomposeEnabled = *req.Decompose
	}
	sub := *s
	sub.cfg = cfg

	sr := &StageResult{Question: question}
	warn := newWarnSet()
	totalStart := time.Now()

	// 1. Decompose.
	subQueries, w := sub.decompose(ctx, question)
	warn.add(w)
	sr.SubQueries = subQueries

	// 2. Per sub-query: embed and fuse.
	groups := make([][]candidate, 0, len(subQueries))
	var denseCount, lexCount int
	for i, q := range subQueries {
		embedStart := time.Now()
		vecs, err := sub.Embedder.Embed(ctx, []string{q})
		if err != nil {
			return nil, fmt.Errorf("embed sub-query %d: %w", i+1, err)
		}
		sr.EmbedDuration += time.Since(embedStart)

		if strings.TrimSpace(q) == "" {
			warn.add(WarnLexicalQueryEmpty)
		}

		denseStart := time.Now()
		lexStart := time.Now()

		cands, err := sub.fuse(ctx, vecs[0], q, i)
		if err != nil {
			return nil, err
		}
		sr.FuseDuration += time.Since(denseStart) // fuse includes both legs in one round trip

		// Count dense and lexical contributions.
		for _, c := range cands {
			if c.DenseRank > 0 {
				denseCount++
			}
			if c.LexRank > 0 {
				lexCount++
			}
		}

		if denseYield(cands) < cfg.VecCandidates && len(cands) < cfg.RerankCandidates {
			warn.add(WarnDenseLegUnderfill)
		}

		groups = append(groups, cands)
		_ = denseStart
		_ = lexStart
	}

	sr.DenseCount = denseCount
	sr.LexicalCount = lexCount

	// Record fused candidates.
	for _, g := range groups {
		for _, c := range g {
			sr.FusedCandidates = append(sr.FusedCandidates, EvalCandidate{
				ChunkID:   c.ChunkID,
				DocID:     c.DocID,
				DenseRank: c.DenseRank,
				LexRank:   c.LexRank,
				RRF:       c.RRF,
			})
		}
	}

	// 3. Pool candidates.
	poolStart := time.Now()
	pool, floored := sub.poolCandidates(groups, cfg.RerankCandidates)
	if floored {
		warn.add(WarnPoolFloorApplied)
	}
	sr.SelectDuration += time.Since(poolStart)

	// 4. Rerank.
	rerankStart := time.Now()
	ranked, w := sub.rerank(ctx, question, pool)
	warn.add(w)
	sr.RerankDuration += time.Since(rerankStart)

	for _, r := range ranked {
		sr.RerankedCandidates = append(sr.RerankedCandidates, r)
	}
	sr.RerankCount = len(ranked)

	// 5. Select.
	selectStart := time.Now()
	sel := sub.selectPackets(ranked, topK)
	if sel.FlooredCount > 0 && len(sel.Picked) < topK {
		warn.add(WarnRelevanceFloorApplied)
	}
	if sel.ThreadCapped {
		warn.add(WarnThreadCapped)
	}
	sr.SelectDuration += time.Since(selectStart)

	// 6. Expand and build packets.
	expandStart := time.Now()
	packets, budget, err := sub.buildPackets(ctx, sel.Picked, subQueries)
	if err != nil {
		return nil, err
	}
	if budget.truncated {
		warn.add(WarnBudgetTruncated)
	}
	sr.ExpandDuration += time.Since(expandStart)

	sr.TotalDuration = time.Since(totalStart)
	sr.SelectedPackets = packets
	sr.Budget = Budget{BytesUsed: budget.used, BytesAllowed: cfg.AnswerContextBytes}
	sr.Warnings = warn.list()
	sr.SelectCount = len(sel.Picked)

	return sr, nil
}

// EvaluateExactSearch runs a brute-force cosine-distance search over the same
// embedding namespace for comparison with HNSW results. This uses a
// transaction-local setting to avoid leaking configuration.
func (s *Service) EvaluateExactSearch(ctx context.Context, vec []float32, embedModel string, limit int) ([]EvalCandidate, float64, error) {
	start := time.Now()

	// Use a dedicated transaction with work_mem set high for the sort.
	tx, err := s.DB.Pool.Begin(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("begin exact search tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Connection-local setting: does not leak to the pool.
	if _, err := tx.Exec(ctx, "SET LOCAL work_mem = '256MB'"); err != nil {
		return nil, 0, fmt.Errorf("set work_mem: %w", err)
	}

	rows, err := tx.Query(ctx, `
SELECT p.chunk_id::text, p.doc_id::text,
       CASE WHEN c.embedding <=> $1::halfvec < 0 THEN 0
            ELSE c.embedding <=> $1::halfvec END AS distance
FROM chunks c JOIN document_chunks p ON p.content_id = c.content_id
WHERE c.embed_model = $2
ORDER BY c.embedding <=> $1::halfvec
LIMIT $3`,
		formatVector(vec), embedModel, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("exact search: %w", err)
	}
	defer rows.Close()

	var cands []EvalCandidate
	for rows.Next() {
		var c EvalCandidate
		var dist float64
		if err := rows.Scan(&c.ChunkID, &c.DocID, &dist); err != nil {
			return nil, 0, err
		}
		c.DenseRank = len(cands) + 1
		c.RRF = 1.0 / (60.0 + float64(c.DenseRank))
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	duration := time.Since(start).Seconds() * 1000
	return cands, duration, nil
}

// EvaluateHNSWSearch runs the normal HNSW search for comparison with exact
// results. Uses a transaction-local ef_search setting.
func (s *Service) EvaluateHNSWSearch(ctx context.Context, vec []float32, embedModel string, limit, efSearch int) ([]EvalCandidate, float64, error) {
	start := time.Now()

	tx, err := s.DB.Pool.Begin(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("begin hnsw search tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if efSearch > 0 {
		if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL hnsw.ef_search = %d", efSearch)); err != nil {
			return nil, 0, fmt.Errorf("set hnsw.ef_search: %w", err)
		}
	}

	rows, err := tx.Query(ctx, `
SELECT p.chunk_id::text, p.doc_id::text,
       c.embedding <=> $1::halfvec AS distance
FROM chunks c JOIN document_chunks p ON p.content_id = c.content_id
WHERE c.embed_model = $2
ORDER BY c.embedding <=> $1::halfvec
LIMIT $3`,
		formatVector(vec), embedModel, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("hnsw search: %w", err)
	}
	defer rows.Close()

	var cands []EvalCandidate
	for rows.Next() {
		var c EvalCandidate
		var dist float64
		if err := rows.Scan(&c.ChunkID, &c.DocID, &dist); err != nil {
			return nil, 0, err
		}
		c.DenseRank = len(cands) + 1
		c.RRF = 1.0 / (60.0 + float64(c.DenseRank))
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	duration := time.Since(start).Seconds() * 1000
	return cands, duration, nil
}

// CorpusSize returns the number of chunks in the workspace.
func (s *Service) CorpusSize(ctx context.Context) (int, error) {
	var n int
	err := s.DB.Pool.QueryRow(ctx,
		`SELECT count(*) FROM document_chunks`).Scan(&n)
	return n, err
}
