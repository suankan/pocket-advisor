package eval

import "testing"

func TestDocumentRecallAtK(t *testing.T) {
	packets := []packetRef{
		{DocID: "11111111-1111-1111-1111-111111111111"},
		{DocID: "22222222-2222-2222-2222-222222222222"},
		{DocID: "33333333-3333-3333-3333-333333333333"},
	}
	expected := []ExpectedDocument{
		{DocumentID: "11111111-1111-1111-1111-111111111111", Grade: 3},
		{DocumentID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Grade: 1},
	}
	if got := DocumentRecallAtK(packets, expected, 2); got != 0.5 {
		t.Errorf("DocumentRecallAtK() = %v, want 0.5", got)
	}
	if got := DocumentRecallAtK([]packetRef{{DocID: expected[0].DocumentID}, {DocID: expected[0].DocumentID}}, expected[:1], 2); got != 1 {
		t.Errorf("DocumentRecallAtK() = %v, want 1 without duplicate credit", got)
	}
}

func TestReciprocalRankFirstExpectedDocument(t *testing.T) {
	packets := []packetRef{{DocID: "other"}, {DocID: "wanted"}}
	expected := []ExpectedDocument{{DocumentID: "wanted", Grade: 2}}
	if got := ReciprocalRankFirstExpectedDocument(packets, expected); got != 0.5 {
		t.Errorf("ReciprocalRankFirstExpectedDocument() = %v, want 0.5", got)
	}
}

func TestNDCGUsesExpectedDocumentGrades(t *testing.T) {
	expected := []ExpectedDocument{{DocumentID: "high", Grade: 3}, {DocumentID: "low", Grade: 1}}
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

func TestForbiddenHits(t *testing.T) {
	packets := []packetRef{{DocID: "a"}, {DocID: "b"}}
	if got := ForbiddenHits(packets, []string{"b"}); got != 1 {
		t.Errorf("ForbiddenHits() = %v, want 1", got)
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
