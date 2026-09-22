package retrieval

import (
	"log/slog"
	"testing"

	"github.com/suankan/pocket-advisor/internal/config"
)

func svc(q config.Query) *Service {
	return &Service{cfg: q, Log: slog.New(slog.DiscardHandler)}
}

func mk(doc string, score float64) scored {
	return scored{candidate: candidate{DocID: doc, ChunkID: doc + "#c", Text: "passage " + doc}, Score: score}
}

// The floor is the reranker's own relevant/not-relevant boundary, not a tuned
// guess: off-domain questions score every candidate below zero, so a query
// with no answer in the corpus must return nothing rather than the
// least-irrelevant fifteen.
func TestRelevanceFloorDropsEverythingWhenNothingIsRelevant(t *testing.T) {
	s := svc(config.Query{MinRelevanceScore: 0})
	ranked := []scored{
		mk("a", -0.030), mk("b", -0.092), mk("c", -0.213),
	}
	sel := s.selectPackets(ranked, 15)
	if len(sel.Picked) != 0 {
		t.Fatalf("got %d packets, want 0", len(sel.Picked))
	}
	if sel.FlooredCount != 3 {
		t.Errorf("floored = %d, want 3", sel.FlooredCount)
	}
}

// The floor is absolute: a below-floor candidate is never returned, not even
// when the requested count leaves a slot unfilled. Returning it would answer a
// question the corpus has no evidence for.
func TestFlooredCandidateNeverBackfills(t *testing.T) {
	s := svc(config.Query{MinRelevanceScore: 0})
	ranked := []scored{
		mk("a", 0.5),
		mk("b", 0.4),
		mk("c", -0.1), // below floor — must not fill the remaining slot
	}
	sel := s.selectPackets(ranked, 15)
	if len(sel.Picked) != 2 {
		t.Fatalf("picked %+v, want only a and b", docIDs(sel.Picked))
	}
	for _, p := range sel.Picked {
		if p.DocID == "c" {
			t.Error("a below-floor candidate was returned")
		}
	}
	if sel.FlooredCount != 1 {
		t.Errorf("FlooredCount = %d, want 1", sel.FlooredCount)
	}
}

func TestOneMatchPerDocument(t *testing.T) {
	s := svc(config.Query{MinRelevanceScore: 0})
	ranked := []scored{mk("a", 0.9), mk("a", 0.8), mk("b", 0.7)}
	sel := s.selectPackets(ranked, 15)
	if got := docIDs(sel.Picked); len(got) != 2 {
		t.Fatalf("got %v, want one entry per document", got)
	}
}

func docIDs(in []scored) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = s.DocID
	}
	return out
}

// Boilerplate is stored as its own chunk in every document that carries it,
// so ten copies of one disclaimer are ten distinct documents that the
// per-document rule cannot collapse.
func TestSelectionReturnsEachDistinctPassageOnce(t *testing.T) {
	s := svc(config.Query{MinRelevanceScore: 0})
	dup := "This message and any attachments are confidential."
	ranked := []scored{
		{candidate: candidate{ChunkID: "c1", DocID: "d1", Text: dup}, Score: 0.9},
		{candidate: candidate{ChunkID: "c2", DocID: "d2", Text: dup}, Score: 0.8},
		{candidate: candidate{ChunkID: "c3", DocID: "d3", Text: dup}, Score: 0.7},
		{candidate: candidate{ChunkID: "c4", DocID: "d4", Text: "the roof was replaced in March"}, Score: 0.6},
	}
	sel := s.selectPackets(ranked, 15)
	if len(sel.Picked) != 2 {
		t.Fatalf("picked %d packets, want 2 distinct passages", len(sel.Picked))
	}
	if sel.Picked[0].ChunkID != "c1" {
		t.Errorf("first pick = %q, want c1 — ranking must not change", sel.Picked[0].ChunkID)
	}
	if sel.Picked[1].ChunkID != "c4" {
		t.Errorf("second pick = %q, want the distinct passage c4", sel.Picked[1].ChunkID)
	}
}

// Extraction reflows the same paragraph differently per document, so the
// duplication that matters is exact only after whitespace is normalised.
func TestSelectionTreatsReflowedTextAsOnePassage(t *testing.T) {
	s := svc(config.Query{MinRelevanceScore: 0})
	ranked := []scored{
		{candidate: candidate{ChunkID: "c1", DocID: "d1", Text: "Licence No. 335666C\nPhone: 0000"}, Score: 0.9},
		{candidate: candidate{ChunkID: "c2", DocID: "d2", Text: "Licence   No. 335666C\n\n  Phone:  0000  "}, Score: 0.8},
	}
	sel := s.selectPackets(ranked, 15)
	if len(sel.Picked) != 1 {
		t.Fatalf("picked %d packets, want 1 after whitespace normalisation", len(sel.Picked))
	}
}

// Near-identical is not identical. Statements and invoices differing only in
// dates or amounts must stay separate, which is why this is equality and not
// a similarity threshold.
func TestSelectionKeepsPassagesDifferingOnlyInDigits(t *testing.T) {
	s := svc(config.Query{MinRelevanceScore: 0})
	ranked := []scored{
		{candidate: candidate{ChunkID: "c1", DocID: "d1", Text: "Statement period 30 April to 30 May. Closing balance 1,204.55"}, Score: 0.9},
		{candidate: candidate{ChunkID: "c2", DocID: "d2", Text: "Statement period 30 May to 30 June. Closing balance 1,318.02"}, Score: 0.8},
	}
	sel := s.selectPackets(ranked, 15)
	if len(sel.Picked) != 2 {
		t.Fatalf("picked %d packets, want 2 — these are distinct statements", len(sel.Picked))
	}
}
