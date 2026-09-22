package retrieval

import (
	"context"
	"sort"
	"strings"
)

// poolCandidates merges every sub-query's candidates into the rerank window.
//
// Filling purely by RRF score would be wrong. RRF favours candidates both legs
// found, which is right when a leg declined and wrong when a leg *could not
// fire*: a chunk ranked 50th by both legs scores 1/110+1/110 = 0.0182 against
// 0.0164 for the best dense-only match. Two situations make that systematic —
// a cross-lingual query, where English terms cannot match Russian text at all,
// and decomposition, where a thinly-represented sub-query gets crowded out by
// another's dual-leg hits (§4.1).
//
// So floors are reserved first, then the remainder fills by score.
//
// Duplicate passages remain in the pool so the reranker receives a stable
// window. Exact duplicate filtering happens during selection, after scoring,
// so it changes returned coverage without changing what the reranker scores.
func (s *Service) poolCandidates(groups [][]candidate, limit int) (pooled []candidate, floored bool) {
	seen := make(map[string]struct{})
	take := func(c candidate) bool {
		if _, dup := seen[c.ChunkID]; dup {
			return false
		}
		seen[c.ChunkID] = struct{}{}
		pooled = append(pooled, c)
		return true
	}

	// Reserve per sub-query, in rank order within each.
	if len(groups) > 1 && s.cfg.PoolFloorPerSubQuery > 0 {
		for _, g := range groups {
			for i, c := range g {
				if i >= s.cfg.PoolFloorPerSubQuery || len(pooled) >= limit {
					break
				}
				if take(c) {
					floored = true
				}
			}
		}
	}

	// Reserve for candidates only the dense leg found.
	if s.cfg.PoolFloorDenseOnly > 0 {
		n := 0
		for _, g := range groups {
			for _, c := range g {
				if n >= s.cfg.PoolFloorDenseOnly || len(pooled) >= limit {
					break
				}
				if c.DenseRank > 0 && c.LexRank == 0 {
					if take(c) {
						n++
						floored = true
					}
				}
			}
		}
	}

	// Everything else by fused score.
	var rest []candidate
	for _, g := range groups {
		rest = append(rest, g...)
	}
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].RRF > rest[j].RRF })
	for _, c := range rest {
		if len(pooled) >= limit {
			break
		}
		take(c)
	}

	// floored only matters if a reservation actually displaced something the
	// score order would not have chosen anyway.
	if len(pooled) < limit {
		floored = false
	}
	return pooled, floored
}

// contentKey identifies a passage by its text, ignoring whitespace.
//
// Equality, not a similarity threshold. Whitespace normalisation handles
// extraction reflow while retaining passages that differ in dates, amounts, or
// other evidence-bearing details.
func contentKey(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// scored is a candidate carrying its reranked relevance.
type scored struct {
	candidate
	Score float64
}

// rerank reorders the pool with the cross-encoder, passing whole passages and
// scoring against the *original* question rather than any sub-query — what
// comes back has to be relevant to what was actually asked.
//
// A reranker outage is not fatal: candidates keep their fused order and the
// degradation is reported. A slightly worse ranking beats no answer.
func (s *Service) rerank(ctx context.Context, question string, pool []candidate) (out []scored, warn string) {
	fallback := func() []scored {
		res := make([]scored, len(pool))
		for i, c := range pool {
			// Fused order preserved; scores left at zero so the relevance
			// floor cannot silently discard everything on a fallback.
			res[i] = scored{candidate: c}
		}
		return res
	}

	if !s.cfg.RerankEnabled || s.Reranker == nil || len(pool) == 0 {
		return fallback(), ""
	}

	docs := make([]string, len(pool))
	for i, c := range pool {
		docs[i] = c.Text
	}

	results, err := s.Reranker.Rerank(ctx, question, docs, len(docs))
	if err != nil {
		s.Log.Warn("reranker unavailable; serving fused order", "error", err)
		return fallback(), WarnRerankerUnavailable
	}

	out = make([]scored, 0, len(results))
	for _, r := range results {
		out = append(out, scored{candidate: pool[r.Index], Score: r.Score})
	}
	return out, ""
}

// selection is the result of applying the floor, per-document dedup and the
// per-thread cap, in that order.
type selection struct {
	Picked       []scored
	FlooredCount int
}

// selectPackets narrows the reranked pool to what is returned.
//
// The relevance floor comes first and is absolute — a below-floor candidate is
// never returned, not even to backfill a slot the thread cap frees. The
// threshold is the reranker's own boundary rather than a tuned guess:
// off-domain questions score every candidate below zero, so the system can
// return nothing and mean it (§5.1).
//
// Then one match per document, best-ranked chunk winning.
//
// Nothing bounds how much of one conversation a single result set may carry.
// The per-thread cap that used to did so on a grouping this system no longer
// derives, and no replacement key was substituted: one email and its
// attachments is a different shape from many messages of one exchange, so a
// container-based cap would not have bounded what the old one bounded. The
// consequence is recorded rather than papered over — see the open decision in
// retrieval-design.md.
//
// Duplicate passages are dropped alongside per-document dedup, and for the
// same reason that rule is not enough on its own: boilerplate is stored as its
// own chunk in every document that carries it, so ten copies of one disclaimer
// are ten distinct documents that seenDoc cannot collapse. Ranking is
// untouched — the first pick is whatever the reranker put first — so this only
// ever replaces a repeat with the next distinct passage.
func (s *Service) selectPackets(ranked []scored, topK int) selection {
	var sel selection
	seenDoc := map[string]struct{}{}
	seenText := map[string]struct{}{}

	for _, r := range ranked {
		if r.Score < s.cfg.MinRelevanceScore {
			sel.FlooredCount++
			continue
		}
		if len(sel.Picked) >= topK {
			continue
		}
		if _, dup := seenDoc[r.DocID]; dup {
			continue
		}
		// An empty key means there is no text to compare, not that every such
		// candidate is the same passage — collapsing those would discard
		// distinct documents on the strength of having nothing to compare.
		textKey := contentKey(r.Text)
		if textKey != "" {
			if _, dup := seenText[textKey]; dup {
				continue
			}
		}
		seenDoc[r.DocID] = struct{}{}
		if textKey != "" {
			seenText[textKey] = struct{}{}
		}
		sel.Picked = append(sel.Picked, r)
	}
	return sel
}
