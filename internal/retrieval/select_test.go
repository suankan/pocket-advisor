package retrieval

import (
	"log/slog"
	"testing"

	"github.com/suankan/pocket-advisor/internal/config"
)

func svc(q config.Query) *Service {
	return &Service{cfg: q, Log: slog.New(slog.DiscardHandler)}
}

func mk(doc, thread string, score float64) scored {
	return scored{candidate: candidate{DocID: doc, ThreadID: thread, ChunkID: doc + "#c"}, Score: score}
}

// The floor is the reranker's own relevant/not-relevant boundary, not a tuned
// guess: off-domain questions score every candidate below zero, so a query
// with no answer in the corpus must return nothing rather than the
// least-irrelevant fifteen.
func TestRelevanceFloorDropsEverythingWhenNothingIsRelevant(t *testing.T) {
	s := svc(config.Query{MinRelevanceScore: 0, MaxPerThread: 3})
	ranked := []scored{
		mk("a", "", -0.030), mk("b", "", -0.092), mk("c", "", -0.213),
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
// to backfill a slot the thread cap freed.
func TestFlooredCandidateNeverBackfills(t *testing.T) {
	s := svc(config.Query{MinRelevanceScore: 0, MaxPerThread: 1})
	ranked := []scored{
		mk("a", "t1", 0.5),
		mk("b", "t1", 0.4), // capped out
		mk("c", "", -0.1),  // below floor — must not take the freed slot
	}
	sel := s.selectPackets(ranked, 15)
	if len(sel.Picked) != 1 || sel.Picked[0].DocID != "a" {
		t.Fatalf("picked %+v, want only a", docIDs(sel.Picked))
	}
	if !sel.ThreadCapped {
		t.Error("expected thread_capped")
	}
}

// 23 messages of one conversation are 23 distinct documents, so per-document
// dedup cannot see thread concentration. Measured on the live corpus, one
// thread took all ten top results.
func TestThreadCapLimitsOneConversation(t *testing.T) {
	s := svc(config.Query{MinRelevanceScore: 0, MaxPerThread: 3})
	var ranked []scored
	for i, d := range []string{"a", "b", "c", "d", "e"} {
		ranked = append(ranked, mk(d, "thread-1", 0.9-float64(i)*0.1))
	}
	ranked = append(ranked, mk("z", "thread-2", 0.1))
	sel := s.selectPackets(ranked, 15)
	if got := len(sel.Picked); got != 4 {
		t.Fatalf("got %d packets, want 4 (3 capped + 1 other thread)", got)
	}
	if sel.Picked[3].DocID != "z" {
		t.Errorf("the other thread should still surface, got %v", docIDs(sel.Picked))
	}
}

// thread_id == "" is the default for anything that never went through email
// threading — every standalone PDF. Capping on it would treat them all as one
// conversation and return three.
func TestEmptyThreadIDIsNotAThread(t *testing.T) {
	s := svc(config.Query{MinRelevanceScore: 0, MaxPerThread: 3})
	var ranked []scored
	for i, d := range []string{"p1", "p2", "p3", "p4", "p5"} {
		ranked = append(ranked, mk(d, "", 0.9-float64(i)*0.1))
	}
	sel := s.selectPackets(ranked, 15)
	if len(sel.Picked) != 5 {
		t.Fatalf("got %d, want all 5: standalone documents are not one thread", len(sel.Picked))
	}
	if sel.ThreadCapped {
		t.Error("empty thread_id must not trip the cap")
	}
}

func TestOneMatchPerDocument(t *testing.T) {
	s := svc(config.Query{MinRelevanceScore: 0, MaxPerThread: 3})
	ranked := []scored{mk("a", "", 0.9), mk("a", "", 0.8), mk("b", "", 0.7)}
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
