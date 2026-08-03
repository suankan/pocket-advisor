// Package retrieval is the read path (retrieval-design.md).
//
// It is deliberately transport-agnostic: no HTTP types cross its API and it
// imports no server package. A CLI mode calls Query directly today; an MCP
// tool or HTTP handler calls the identical function later (§7).
package retrieval

import (
	"context"
	"strings"
)

// decomposePrompt splits a question into independent search queries.
//
// The no-op case is handled by the model rather than by a classifier in front
// of it: a single-topic question comes back unchanged, measured at ~0.9s, so
// there is nothing to decide before calling.
const decomposePrompt = `Split the question into independent search queries. If it asks one thing, output it unchanged.
Output only queries, one per line. No explanation.

Question: %s
Queries:`

// decompose returns the queries to actually run.
//
// A single embedding of a multi-topic question lands *between* its topics
// rather than on either — measured, a two-topic question returned zero
// documents for one of them while its sub-queries returned both (§3.6).
//
// Failure is never fatal: the original question is a valid single sub-query,
// so a decomposer outage degrades ranking rather than the service.
func (s *Service) decompose(ctx context.Context, question string) (subs []string, warn string) {
	if !s.cfg.DecomposeEnabled || s.LLM == nil {
		return []string{question}, ""
	}

	out, err := s.LLM.Complete(ctx, sprintf(decomposePrompt, question), 200)
	if err != nil {
		s.Log.Warn("decomposition failed; using the original question",
			"error", err)
		return []string{question}, WarnDecompositionUnavailable
	}

	subs = dedupeQueries(splitLines(out), s.cfg.MaxSubQueries)
	if len(subs) == 0 {
		return []string{question}, WarnDecompositionUnavailable
	}
	return subs, ""
}

func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		// Models sometimes number or bullet their output despite instructions.
		l = strings.TrimLeft(l, "-*0123456789. \t")
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// dedupeQueries collapses repeats and bounds the fan-out.
//
// Both guards are earned. Decomposition has been observed emitting the same
// sub-query twice and producing a superset alongside its own parts; duplicates
// matter beyond wasted work because the per-sub-query pool floors (§4.1) would
// then reserve slots several times over for one topic, amplifying redundancy
// instead of protecting diversity.
func dedupeQueries(in []string, max int) []string {
	if max <= 0 {
		max = 4
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, q := range in {
		key := strings.ToLower(strings.Join(strings.Fields(q), " "))
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, q)
		if len(out) >= max {
			break
		}
	}
	return out
}
