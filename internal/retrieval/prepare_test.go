package retrieval

import (
	"strings"
	"testing"
)

// Decomposition has been observed emitting the same sub-query twice and a
// superset alongside its own parts. Duplicates matter beyond wasted fan-out:
// the per-sub-query pool floors would reserve slots several times over for one
// topic, amplifying redundancy instead of protecting diversity.
func TestDedupeQueriesCollapsesRepeats(t *testing.T) {
	got := dedupeQueries([]string{
		"solicitor drug testing",
		"Solicitor  Drug   Testing",
		"solicitor cruise",
	}, 4)
	if len(got) != 2 {
		t.Fatalf("got %q, want 2 distinct", got)
	}
}

func TestDedupeQueriesBoundsFanOut(t *testing.T) {
	got := dedupeQueries([]string{"a", "b", "c", "d", "e", "f"}, 4)
	if len(got) != 4 {
		t.Fatalf("got %d, want the cap of 4", len(got))
	}
}

// Models number or bullet their output despite instructions.
func TestSplitLinesStripsListMarkers(t *testing.T) {
	got := splitLines("1. closing balance\n- solicitor parenting\n\n  * cruise  \n")
	want := []string{"closing balance", "solicitor parenting", "cruise"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBudgeterSharesOneAllowanceAcrossPackets(t *testing.T) {
	b := newBudgeter(100)
	if _, ok := b.take(strings.Repeat("x", 60)); !ok {
		t.Fatal("first take should fit")
	}
	if _, ok := b.take(strings.Repeat("x", 60)); ok {
		t.Fatal("second take should not fit — the allowance is shared, not per packet")
	}
	if !b.truncated {
		t.Error("truncation must be reported, not silent")
	}
	if b.used != 60 {
		t.Errorf("used = %d, want 60", b.used)
	}
}

// Omitted text is not omitted provenance: a neighbour that does not fit keeps
// its identity so a reader can pull it manually.
func TestBudgeterEmptyTextAlwaysFits(t *testing.T) {
	b := newBudgeter(0)
	if _, ok := b.take(""); !ok {
		t.Error("empty text should not count as truncation")
	}
	if b.truncated {
		t.Error("empty text must not set truncated")
	}
}

func TestWarnSetIsUniqueAndOrdered(t *testing.T) {
	w := newWarnSet()
	w.add(WarnThreadCapped)
	w.add("")
	w.add(WarnThreadCapped)
	w.add(WarnBudgetTruncated)
	got := w.list()
	if len(got) != 2 || got[0] != WarnThreadCapped || got[1] != WarnBudgetTruncated {
		t.Fatalf("got %q", got)
	}
}

func TestFormatVectorIsPgvectorText(t *testing.T) {
	if got := formatVector([]float32{1, -0.5, 0.25}); got != "[1,-0.5,0.25]" {
		t.Fatalf("got %q", got)
	}
}
