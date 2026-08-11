package retrieval

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/suankan/pocket-advisor/internal/client/embedding"
	"github.com/suankan/pocket-advisor/internal/client/llm"
	"github.com/suankan/pocket-advisor/internal/client/reranking"
	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/topicgraph"
)

// Warnings surfaced on a result. Silent degradation is the dominant failure
// mode of a retrieval system: every mechanism that can quietly reduce quality
// reports itself here rather than in a log (§7.1).
const (
	WarnDenseLegUnderfill        = "dense_leg_underfill"
	WarnLexicalQueryEmpty        = "lexical_query_empty"
	WarnDecompositionUnavailable = "decomposition_unavailable"
	WarnPoolFloorApplied         = "pool_floor_applied"
	WarnRerankerUnavailable      = "reranker_unavailable"
	WarnRelevanceFloorApplied    = "relevance_floor_applied"
	WarnThreadCapped             = "thread_capped"
	WarnBudgetTruncated          = "budget_truncated"
	WarnTopicGraphUnavailable    = "topic_graph_unavailable"
)

// Service holds warm clients and nothing else. All state is in PostgreSQL.
//
// Constructed once and injected: rebuilding model clients per request would
// reintroduce the cost a warm daemon exists to avoid. Note the expensive
// warmth is the model server's, not ours — this binary starts in ~20ms.
type Service struct {
	DB       *postgres.DB
	Embedder *embedding.Client
	Reranker *reranking.Client
	LLM      *llm.Client
	Log      *slog.Logger

	cfg       config.Query
	workspace string
	timeline  topicTimeline
}

// New wires a Service and asserts its scope.
func New(
	db *postgres.DB, emb *embedding.Client, rr *reranking.Client, l *llm.Client,
	cfg config.Query, workspace string, log *slog.Logger,
) *Service {
	service := &Service{
		DB: db, Embedder: emb, Reranker: rr, LLM: l,
		cfg: cfg, workspace: workspace, Log: log,
	}
	// Construction cannot fail for a non-empty fixed workspace. Keep the
	// optional graph collaborator separate so disabled ordinary retrieval never
	// opens a graph snapshot or consults derived data.
	if timeline, err := topicgraph.NewTimelineService(postgres.NewTopicTimelineStore(db), workspace); err == nil {
		service.timeline = timeline
	}
	return service
}

// AssertScope refuses to serve if the connected database holds anything but
// the expected workspace.
//
// The fusion query carries no workspace predicate, because each workspace is
// its own database and the predicate would match every row. A per-query filter
// that is always true gives false comfort: it would silently *hide* foreign
// data rather than reveal that it should not be there. This checks once, at
// startup, and fails loudly instead (§3.4).
func (s *Service) AssertScope(ctx context.Context) error {
	rows, err := s.DB.Pool.Query(ctx,
		`SELECT DISTINCT workspace_id FROM chunks LIMIT 5`)
	if err != nil {
		return fmt.Errorf("assert workspace scope: %w", err)
	}
	defer rows.Close()

	var found []string
	for rows.Next() {
		var w string
		if err := rows.Scan(&w); err != nil {
			return err
		}
		found = append(found, w)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	switch {
	case len(found) == 0:
		return nil // empty index is not a scope violation
	case len(found) > 1:
		return fmt.Errorf("database holds %d workspaces (%s); it must hold exactly one",
			len(found), strings.Join(found, ", "))
	case found[0] != s.workspace:
		return fmt.Errorf("database holds workspace %q but %q was requested",
			found[0], s.workspace)
	}
	return nil
}

// Request is what a caller asks for. No transport types, by design (§7).
type Request struct {
	Question  string
	TopK      int
	Rerank    *bool
	Decompose *bool
}

// Result is the deliverable. Packets are complete on their own — the consumer
// is a human, or an agent that performs generation outside this codebase
// (§6.1).
type Result struct {
	Question       string          `json:"question"`
	SubQueries     []string        `json:"sub_queries"`
	Packets        []Packet        `json:"packets"`
	Warnings       []string        `json:"warnings"`
	Budget         Budget          `json:"budget"`
	GraphExpansion *GraphExpansion `json:"graph_expansion,omitempty"`
}

type Budget struct {
	BytesUsed    int `json:"bytes_used"`
	BytesAllowed int `json:"bytes_allowed"`
}

// Query runs the read path end to end.
func (s *Service) Query(ctx context.Context, req Request) (*Result, error) {
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

	res := &Result{Question: question, Budget: Budget{BytesAllowed: cfg.AnswerContextBytes}}
	warn := newWarnSet()

	// 1. Prepare: split a multi-topic question so one vector does not land
	//    between its topics (§3.6).
	subQueries, w := sub.decompose(ctx, question)
	warn.add(w)
	res.SubQueries = subQueries

	// 2. Per sub-query: embed and fuse. Both are cheap (~23ms + ~5ms), which
	//    is what makes fanning out and reranking once affordable.
	groups := make([][]candidate, 0, len(subQueries))
	for i, q := range subQueries {
		vecs, err := sub.Embedder.Embed(ctx, []string{q})
		if err != nil {
			return nil, fmt.Errorf("embed sub-query %d: %w", i+1, err)
		}
		// BM25 tokenises the raw text itself (fuse.go), so there is no
		// separate query-construction step left to fail — only the
		// degenerate case of nothing to tokenise at all.
		if strings.TrimSpace(q) == "" {
			warn.add(WarnLexicalQueryEmpty)
		}
		cands, err := sub.fuse(ctx, vecs[0], q, i)
		if err != nil {
			return nil, err
		}
		if denseYield(cands) < cfg.VecCandidates && len(cands) < cfg.RerankCandidates {
			warn.add(WarnDenseLegUnderfill)
		}
		groups = append(groups, cands)
	}

	// 3. Union with reserved floors, so a leg or sub-query that cannot compete
	//    on fused score is not silently erased (§4.1).
	pool, floored := sub.poolCandidates(groups, cfg.RerankCandidates)
	if floored {
		warn.add(WarnPoolFloorApplied)
	}

	// 4. Rerank once, against the original question.
	ranked, w := sub.rerank(ctx, question, pool)
	warn.add(w)

	// 5. Relevance floor, one per document, thread cap.
	sel := sub.selectPackets(ranked, topK)
	if sel.FlooredCount > 0 && len(sel.Picked) < topK {
		warn.add(WarnRelevanceFloorApplied)
	}
	if sel.ThreadCapped {
		warn.add(WarnThreadCapped)
	}

	// 6. Expand through lineage and pack against the shared budget.
	packets, budget, err := sub.buildPackets(ctx, sel.Picked, subQueries)
	if err != nil {
		return nil, err
	}
	graph, graphWarnings := sub.expandTopicGraph(ctx, sel.Picked, budget)
	warn.addAll(graphWarnings)
	if budget.truncated {
		warn.add(WarnBudgetTruncated)
	}
	res.GraphExpansion = graph
	// Always emit arrays rather than nulls. A zero-packet answer is a real,
	// correct outcome — an off-domain question should return nothing — and a
	// consumer should not have to distinguish "no results" from "field absent".
	res.Packets = nonNilPackets(packets)
	res.Budget.BytesUsed = budget.used
	res.Warnings = nonNilStrings(warn.list())
	res.SubQueries = nonNilStrings(res.SubQueries)
	return res, nil
}

func nonNilPackets(p []Packet) []Packet {
	if p == nil {
		return []Packet{}
	}
	return p
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func denseYield(cands []candidate) int {
	n := 0
	for _, c := range cands {
		if c.DenseRank > 0 {
			n++
		}
	}
	return n
}

// warnSet keeps warnings unique and in a stable order.
type warnSet struct {
	seen  map[string]struct{}
	order []string
}

func newWarnSet() *warnSet { return &warnSet{seen: map[string]struct{}{}} }

func (w *warnSet) add(s string) {
	if s == "" {
		return
	}
	if _, dup := w.seen[s]; dup {
		return
	}
	w.seen[s] = struct{}{}
	w.order = append(w.order, s)
}

func (w *warnSet) addAll(values []string) {
	for _, value := range values {
		w.add(value)
	}
}

func (w *warnSet) list() []string { return w.order }

func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }
