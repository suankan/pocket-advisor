package eval

import "testing"

func TestFactRecallIgnoresLayoutWhitespace(t *testing.T) {
	// Exactly the shape extracted PDF text arrives in: a table row rendered
	// on a character grid, with runs of spaces standing in for columns.
	packets := []packetRef{{DocID: "a", Text: "31/03/22   CLOSING   BALANCE\n\n                45.59"}}
	facts := []ExpectedFact{{Span: "31/03/22 CLOSING BALANCE 45.59", Grade: 3}}

	if got := FactRecall(packets, facts); got != 1 {
		t.Errorf("FactRecall() = %v, want 1 under whitespace-collapsed matching", got)
	}
}

// The case document recall cannot see. The right document is ranked first,
// and the shared budget then drops its text, so nothing in the response
// grounds the answer.
func TestFactRecallFailsWhenBudgetDroppedTheText(t *testing.T) {
	packets := []packetRef{{DocID: "a", Text: ""}}
	facts := []ExpectedFact{{Span: "Hours Paid: 43.47", Grade: 3}}

	if got := DocumentRecallAtK(packets, []ResolvedDocument{{DocID: "a", Grade: 3}}, 10); got != 1 {
		t.Fatalf("DocumentRecallAtK() = %v, want 1 — the document was ranked", got)
	}
	if got := FactRecall(packets, facts); got != 0 {
		t.Errorf("FactRecall() = %v, want 0 — its text was never returned", got)
	}
}

func TestFactRecallCountsEachFactOnce(t *testing.T) {
	packets := []packetRef{
		{DocID: "a", Text: "the total offset benefit is -$14.64"},
		{DocID: "b", Text: "the total offset benefit is -$14.64 as well"},
	}
	facts := []ExpectedFact{
		{Span: "total offset benefit is -$14.64", Grade: 3},
		{Span: "a fact nothing grounds", Grade: 3},
	}
	if got := FactRecall(packets, facts); got != 0.5 {
		t.Errorf("FactRecall() = %v, want 0.5", got)
	}
}

// Facts may be grounded by a related document delivered alongside the match,
// so recall is not tied to which packet supplied the text — but the rank of
// the first grounding packet still is.
func TestReciprocalRankFirstFactUsesTheFirstGroundingPacket(t *testing.T) {
	packets := []packetRef{
		{DocID: "a", Text: "unrelated"},
		{DocID: "b", Text: "Loan Expiry Date 15 March 2052"},
	}
	facts := []ExpectedFact{{Span: "Loan Expiry Date 15 March 2052", Grade: 3}}
	if got := ReciprocalRankFirstFact(packets, facts); got != 0.5 {
		t.Errorf("ReciprocalRankFirstFact() = %v, want 0.5", got)
	}
	if got := ReciprocalRankFirstFact(packets, []ExpectedFact{{Span: "absent", Grade: 3}}); got != 0 {
		t.Errorf("ReciprocalRankFirstFact() = %v, want 0 when nothing grounds the fact", got)
	}
}

// A question about styling has to be able to tell iStem from iSTEM, which are
// different answers to "which option uses a lowercase i".
func TestFactMatchingHonoursCaseSensitivity(t *testing.T) {
	packets := []packetRef{{DocID: "a", Text: "electives: iSTEM, Performing Arts"}}
	insensitive := []ExpectedFact{{Span: "iStem", Grade: 3}}
	sensitive := []ExpectedFact{{Span: "iStem", Grade: 3, CaseSensitive: true}}

	if got := FactRecall(packets, insensitive); got != 1 {
		t.Errorf("FactRecall(case-insensitive) = %v, want 1", got)
	}
	if got := FactRecall(packets, sensitive); got != 0 {
		t.Errorf("FactRecall(case-sensitive) = %v, want 0 — iSTEM is a different answer", got)
	}
}

func TestFactMetricsAreNeutralWithoutFacts(t *testing.T) {
	packets := []packetRef{{DocID: "a", Text: "anything"}}
	if got := FactRecall(packets, nil); got != 1 {
		t.Errorf("FactRecall(no facts) = %v, want 1", got)
	}
	if got := ReciprocalRankFirstFact(packets, nil); got != 1 {
		t.Errorf("ReciprocalRankFirstFact(no facts) = %v, want 1", got)
	}
}

// The answer's position is observed, never asserted: depth is a trend to
// watch across runs, not a per-case contract.
func TestFirstFactRankReportsPosition(t *testing.T) {
	packets := []packetRef{
		{DocID: "a", Text: "unrelated"},
		{DocID: "b", Text: ""},
		{DocID: "c", Text: "Hours Paid: 43.47"},
	}
	facts := []ExpectedFact{{Span: "Hours Paid: 43.47", Grade: 3}}
	if got := FirstFactRank(packets, facts); got != 3 {
		t.Errorf("FirstFactRank() = %v, want 3", got)
	}
	if got := FirstFactRank(packets, []ExpectedFact{{Span: "absent", Grade: 3}}); got != 0 {
		t.Errorf("FirstFactRank() = %v, want 0 when nothing grounds the fact", got)
	}
	if got := FirstFactRank(packets, nil); got != 0 {
		t.Errorf("FirstFactRank(no facts) = %v, want 0", got)
	}
}
