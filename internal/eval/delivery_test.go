package eval

import "testing"

// The failure plain document recall structurally cannot see: the right
// document is ranked, and the shared budget then drops its text, so the
// caller receives a title and an empty body and cannot answer from it.
func TestDeliveredRecallSeparatesFoundFromReturned(t *testing.T) {
	expected := []ResolvedDocument{{DocID: "a", Grade: 3}}
	ranked := []packetRef{{DocID: "a", Text: ""}}

	if got := DocumentRecallAtK(ranked, expected, 10); got != 1 {
		t.Fatalf("DocumentRecallAtK() = %v, want 1 — the document was found", got)
	}
	if got := DeliveredRecallAtK(ranked, expected, 10); got != 0 {
		t.Errorf("DeliveredRecallAtK() = %v, want 0 — its text never arrived", got)
	}
	if got := FirstDeliveredRank(ranked, expected); got != 0 {
		t.Errorf("FirstDeliveredRank() = %v, want 0", got)
	}
	if got := FirstExpectedDocumentRank(ranked, expected); got != 1 {
		t.Errorf("FirstExpectedDocumentRank() = %v, want 1", got)
	}
}

func TestDeliveredRecallCountsOnlyDocumentsWithText(t *testing.T) {
	expected := []ResolvedDocument{{DocID: "a", Grade: 3}, {DocID: "b", Grade: 3}}
	ranked := []packetRef{{DocID: "a", Text: "the statement"}, {DocID: "b", Text: ""}}

	if got := DeliveredRecallAtK(ranked, expected, 10); got != 0.5 {
		t.Errorf("DeliveredRecallAtK() = %v, want 0.5", got)
	}
}

func TestDeliveredRankSkipsUndeliveredMatches(t *testing.T) {
	expected := []ResolvedDocument{{DocID: "a", Grade: 3}, {DocID: "b", Grade: 3}}
	ranked := []packetRef{
		{DocID: "other", Text: "x"},
		{DocID: "a", Text: ""},              // found, not delivered
		{DocID: "b", Text: "the statement"}, // the first one a caller can use
	}
	if got := FirstExpectedDocumentRank(ranked, expected); got != 2 {
		t.Errorf("FirstExpectedDocumentRank() = %v, want 2", got)
	}
	if got := FirstDeliveredRank(ranked, expected); got != 3 {
		t.Errorf("FirstDeliveredRank() = %v, want 3", got)
	}
	if got := ReciprocalRankFirstDelivered(ranked, expected); got < 0.33 || got > 0.34 {
		t.Errorf("ReciprocalRankFirstDelivered() = %v, want about 1/3", got)
	}
}

// An expected-empty case asserts nothing about documents, so the delivery
// metrics must not drag its run's averages down.
func TestDeliveryMetricsAreNeutralWithoutExpectedDocuments(t *testing.T) {
	ranked := []packetRef{{DocID: "a", Text: "anything"}}
	if got := DeliveredRecallAtK(ranked, nil, 10); got != 1 {
		t.Errorf("DeliveredRecallAtK(no expected) = %v, want 1", got)
	}
	if got := ReciprocalRankFirstDelivered(ranked, nil); got != 1 {
		t.Errorf("ReciprocalRankFirstDelivered(no expected) = %v, want 1", got)
	}
}

// Beyond top-k a document is not part of the result a caller receives.
func TestDeliveredRecallRespectsK(t *testing.T) {
	expected := []ResolvedDocument{{DocID: "b", Grade: 3}}
	ranked := []packetRef{{DocID: "a", Text: "x"}, {DocID: "b", Text: "the statement"}}
	if got := DeliveredRecallAtK(ranked, expected, 1); got != 0 {
		t.Errorf("DeliveredRecallAtK(k=1) = %v, want 0", got)
	}
	if got := DeliveredRecallAtK(ranked, expected, 2); got != 1 {
		t.Errorf("DeliveredRecallAtK(k=2) = %v, want 1", got)
	}
}

// The attribution failure: the budget refused the expected document's own
// text and admitted its covering email instead. The caller received a title,
// an empty body, and somebody else's prose — which is not the document.
func TestPrimaryDeliveryNeverBorrowsNeighbourText(t *testing.T) {
	expected := []ResolvedDocument{{DocID: "statement", Grade: 3}}
	ranked := []packetRef{{
		DocID:   "statement",
		Text:    "",
		Related: []relatedRef{{DocID: "covering-email", Text: "please find attached"}},
	}}

	if got := DeliveredRecallAtK(ranked, expected, 10); got != 0 {
		t.Errorf("DeliveredRecallAtK() = %v, want 0 — the document's own text never arrived", got)
	}
	if got := FirstDeliveredRank(ranked, expected); got != 0 {
		t.Errorf("FirstDeliveredRank() = %v, want 0", got)
	}
	if got := UnionDeliveredRecallAtK(ranked, expected, 10); got != 0 {
		t.Errorf("UnionDeliveredRecallAtK() = %v, want 0 — the neighbour is a different document", got)
	}
}

// Expected evidence that arrives only as somebody else's neighbour is real
// delivery, but supplementary delivery, and must be reported as such.
func TestExpectedEvidenceDeliveredOnlyAsNeighbour(t *testing.T) {
	expected := []ResolvedDocument{{DocID: "statement", Grade: 3}}
	ranked := []packetRef{{
		DocID:   "covering-email",
		Text:    "please find attached",
		Related: []relatedRef{{DocID: "statement", Text: "closing balance"}},
	}}

	if got := DeliveredRecallAtK(ranked, expected, 10); got != 0 {
		t.Errorf("DeliveredRecallAtK() = %v, want 0 — it was never a packet of its own", got)
	}
	if got := SupplementaryDeliveredRecallAtK(ranked, expected, 10); got != 1 {
		t.Errorf("SupplementaryDeliveredRecallAtK() = %v, want 1", got)
	}
	if got := UnionDeliveredRecallAtK(ranked, expected, 10); got != 1 {
		t.Errorf("UnionDeliveredRecallAtK() = %v, want 1 — the caller did receive it", got)
	}
}

// A neighbour the budget dropped delivers nothing, however it is counted.
func TestOmittedNeighbourDeliversNothing(t *testing.T) {
	expected := []ResolvedDocument{{DocID: "statement", Grade: 3}}
	ranked := []packetRef{{
		DocID:   "covering-email",
		Text:    "please find attached",
		Related: []relatedRef{{DocID: "statement", Text: ""}},
	}}

	if got := SupplementaryDeliveredRecallAtK(ranked, expected, 10); got != 0 {
		t.Errorf("SupplementaryDeliveredRecallAtK() = %v, want 0", got)
	}
	if got := UnionDeliveredRecallAtK(ranked, expected, 10); got != 0 {
		t.Errorf("UnionDeliveredRecallAtK() = %v, want 0", got)
	}
}

// One document delivered as a packet and repeated as another packet's
// neighbour is one delivery, and belongs to the primary side.
func TestDuplicatePrimaryAndRelatedIdentityCountsOnce(t *testing.T) {
	expected := []ResolvedDocument{{DocID: "statement", Grade: 3}}
	ranked := []packetRef{
		{DocID: "statement", Text: "closing balance"},
		{DocID: "covering-email", Text: "please find attached",
			Related: []relatedRef{{DocID: "statement", Text: "closing balance"}}},
	}

	if got := DeliveredRecallAtK(ranked, expected, 10); got != 1 {
		t.Errorf("DeliveredRecallAtK() = %v, want 1", got)
	}
	if got := SupplementaryDeliveredRecallAtK(ranked, expected, 10); got != 0 {
		t.Errorf("SupplementaryDeliveredRecallAtK() = %v, want 0 — it is already a primary delivery", got)
	}
	if got := UnionDeliveredRecallAtK(ranked, expected, 10); got != 1 {
		t.Errorf("UnionDeliveredRecallAtK() = %v, want 1, not 2", got)
	}
}
