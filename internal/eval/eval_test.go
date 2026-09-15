package eval

import "testing"

func TestDocumentRecallAtK(t *testing.T) {
	packets := []packetRef{
		{DocID: "11111111-1111-1111-1111-111111111111"},
		{DocID: "22222222-2222-2222-2222-222222222222"},
		{DocID: "33333333-3333-3333-3333-333333333333"},
	}
	expected := []ResolvedDocument{
		{DocID: "11111111-1111-1111-1111-111111111111", Grade: 3},
		{DocID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Grade: 1},
	}
	if got := DocumentRecallAtK(packets, expected, 2); got != 0.5 {
		t.Errorf("DocumentRecallAtK() = %v, want 0.5", got)
	}
	if got := DocumentRecallAtK([]packetRef{{DocID: expected[0].DocID}, {DocID: expected[0].DocID}}, expected[:1], 2); got != 1 {
		t.Errorf("DocumentRecallAtK() = %v, want 1 without duplicate credit", got)
	}
}

func TestReciprocalRankFirstExpectedDocument(t *testing.T) {
	packets := []packetRef{{DocID: "other"}, {DocID: "wanted"}}
	expected := []ResolvedDocument{{DocID: "wanted", Grade: 2}}
	if got := ReciprocalRankFirstExpectedDocument(packets, expected); got != 0.5 {
		t.Errorf("ReciprocalRankFirstExpectedDocument() = %v, want 0.5", got)
	}
}

func TestNDCGUsesExpectedDocumentGrades(t *testing.T) {
	expected := []ResolvedDocument{{DocID: "high", Grade: 3}, {DocID: "low", Grade: 1}}
	if got := NDCG([]packetRef{{DocID: "high"}, {DocID: "low"}}, expected); got < 0.99 || got > 1.01 {
		t.Errorf("NDCG() = %v, want 1 for ideal grade order", got)
	}
	if got := NDCG([]packetRef{{DocID: "low"}, {DocID: "high"}}, expected); got >= 1 {
		t.Errorf("NDCG() = %v, want less than 1 for nonideal grade order", got)
	}
	if got := NDCG([]packetRef{{DocID: "high"}, {DocID: "high"}}, expected[:1]); got > 1.0+1e-9 {
		t.Errorf("NDCG() = %v, want no duplicate credit", got)
	}
}

func TestFirstExpectedDocumentRankReportsPosition(t *testing.T) {
	packets := []packetRef{{DocID: "other"}, {DocID: "wanted"}}
	expected := []ResolvedDocument{{DocID: "wanted", Grade: 2}}
	if got := FirstExpectedDocumentRank(packets, expected); got != 2 {
		t.Errorf("FirstExpectedDocumentRank() = %v, want 2", got)
	}
	// Absent and last are different outcomes and must not both read as a
	// large number.
	if got := FirstExpectedDocumentRank(packets, []ResolvedDocument{{DocID: "missing"}}); got != 0 {
		t.Errorf("FirstExpectedDocumentRank() = %v, want 0 when never returned", got)
	}
}

func TestDistributionFrom(t *testing.T) {
	got := DistributionFrom([]float64{10, 20, 30, 40, 50})
	if got.N != 5 || got.Min != 10 || got.Max != 50 || got.Mean != 30 {
		t.Errorf("DistributionFrom() = %+v, want five values with 10/50/30 min/max/mean", got)
	}
}

func TestFilterCases(t *testing.T) {
	cases := []Case{{ID: "c1", Category: "a"}, {ID: "c2", Category: "b"}}
	if got := FilterCases(cases, []string{"c1"}, []string{"a"}); len(got) != 1 || got[0].ID != "c1" {
		t.Errorf("FilterCases() = %v, want c1", got)
	}
}
