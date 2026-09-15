package retrieval

import (
	"context"
	"fmt"
	"time"
)

// StageResult is the evaluation-only view of one retrieval run.
type StageResult struct {
	// Result is exactly what a caller receives, not a copy of the fields
	// evaluation happened to need. Embedding it means a field added to the
	// production result is visible to evaluation by construction, rather
	// than through someone remembering to mirror it here — which is how
	// graph expansion came to be invisible to evaluation for a while.
	*Result

	// Per-stage candidate counts and the intermediate candidates behind them.
	FusedCandidates    []EvalCandidate
	RerankedCandidates []scored

	// Latency per stage. Dense and lexical overlap deliberately: both legs
	// are computed in one SQL statement, so their timings are the same
	// measurement reported under two names rather than additive costs.
	EmbedDuration   time.Duration
	DenseDuration   time.Duration
	LexicalDuration time.Duration
	FuseDuration    time.Duration
	RerankDuration  time.Duration
	SelectDuration  time.Duration
	ExpandDuration  time.Duration
	TotalDuration   time.Duration

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

// stage names a measured step of the read path.
type stage int

const (
	stageTotal stage = iota
	stageEmbed
	stageFusion
	stageRerank
	stageSelection
	stageExpand
)

// runObservation collects what evaluation needs and production discards.
//
// Every method tolerates a nil receiver and does nothing, which is what lets
// one orchestration serve both callers: Query passes nil and pays a pointer
// comparison per stage, never allocating a candidate copy and never reaching
// time.Now at all.
type runObservation struct {
	total, embed, fusion, rerank, selection, expand time.Duration

	fused    []EvalCandidate
	ranked   []scored
	selected int
}

// noopDone is returned by begin on a nil observation so the call site can
// stay unconditional without allocating a closure per stage.
var noopDone = func() {}

// begin starts timing a stage and returns the function that records it.
func (o *runObservation) begin(st stage) func() {
	if o == nil {
		return noopDone
	}
	at := time.Now()
	return func() {
		d := time.Since(at)
		switch st {
		case stageTotal:
			o.total += d
		case stageEmbed:
			o.embed += d
		case stageFusion:
			o.fusion += d
		case stageRerank:
			o.rerank += d
		case stageSelection:
			o.selection += d
		case stageExpand:
			o.expand += d
		}
	}
}

func (o *runObservation) recordFused(groups [][]candidate) {
	if o == nil {
		return
	}
	for _, g := range groups {
		for _, c := range g {
			o.fused = append(o.fused, EvalCandidate{
				ChunkID:   c.ChunkID,
				DocID:     c.DocID,
				DenseRank: c.DenseRank,
				LexRank:   c.LexRank,
				RRF:       c.RRF,
			})
		}
	}
}

func (o *runObservation) recordRanked(ranked []scored) {
	if o == nil {
		return
	}
	o.ranked = append(o.ranked, ranked...)
}

func (o *runObservation) recordSelected(n int) {
	if o == nil {
		return
	}
	o.selected = n
}

// stageResult projects the observation, alongside the result the caller would
// have received, into the shape evaluation reports on.
func (o *runObservation) stageResult(res *Result) *StageResult {
	sr := &StageResult{
		Result:             res,
		FusedCandidates:    o.fused,
		RerankedCandidates: o.ranked,
		EmbedDuration:      o.embed,
		DenseDuration:      o.fusion,
		LexicalDuration:    o.fusion,
		FuseDuration:       o.fusion,
		RerankDuration:     o.rerank,
		SelectDuration:     o.selection,
		ExpandDuration:     o.expand,
		TotalDuration:      o.total,
		FusedCount:         len(o.fused),
		RerankCount:        len(o.ranked),
		SelectCount:        o.selected,
	}
	for _, c := range o.fused {
		if c.DenseRank > 0 {
			sr.DenseCount++
		}
		if c.LexRank > 0 {
			sr.LexicalCount++
		}
	}
	return sr
}

// Evaluate runs the same read path Query runs and additionally reports its
// intermediate stages. It adds observation; it does not re-implement the
// pipeline, so there is no ordering that can drift from production.
func (s *Service) Evaluate(ctx context.Context, req Request) (*StageResult, error) {
	obs := &runObservation{}
	res, err := s.run(ctx, req, obs)
	if err != nil {
		return nil, err
	}
	return obs.stageResult(res), nil
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
SELECT p.placement_id::text, p.doc_id::text,
       CASE WHEN c.embedding <=> $1::halfvec < 0 THEN 0
            ELSE c.embedding <=> $1::halfvec END AS distance
FROM chunks c JOIN document_chunks p ON p.chunk_id = c.chunk_id
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
SELECT p.placement_id::text, p.doc_id::text,
       c.embedding <=> $1::halfvec AS distance
FROM chunks c JOIN document_chunks p ON p.chunk_id = c.chunk_id
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
