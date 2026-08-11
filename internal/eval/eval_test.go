package eval

import (
	"testing"
)

func TestSourceRecallAtK(t *testing.T) {
	packets := []packetRef{
		{DocID: "a", FixtureID: "doc-a"},
		{DocID: "b", FixtureID: "doc-b"},
		{DocID: "c", FixtureID: "doc-c"},
		{DocID: "d", FixtureID: "doc-d"},
	}

	tests := []struct {
		name     string
		expected []ExpectedSource
		k        int
		want     float64
	}{
		{
			name:     "all found",
			expected: []ExpectedSource{{FixtureID: "doc-a"}, {FixtureID: "doc-b"}},
			k:        2,
			want:     1.0,
		},
		{
			name:     "partial found",
			expected: []ExpectedSource{{FixtureID: "doc-a"}, {FixtureID: "doc-x"}},
			k:        2,
			want:     0.5,
		},
		{
			name:     "none found",
			expected: []ExpectedSource{{FixtureID: "doc-x"}, {FixtureID: "doc-y"}},
			k:        4,
			want:     0.0,
		},
		{
			name:     "empty expected",
			expected: []ExpectedSource{},
			k:        4,
			want:     1.0,
		},
		{
			name:     "k limits search",
			expected: []ExpectedSource{{FixtureID: "doc-d"}},
			k:        2,
			want:     0.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SourceRecallAtK(packets, tt.expected, tt.k)
			if got != tt.want {
				t.Errorf("SourceRecallAtK() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSourceRecallAtKDedupesRepeatedSource guards against a real bug: a
// source split across several selected chunks must count once, not once per
// chunk, or recall can exceed 1.0.
func TestSourceRecallAtKDedupesRepeatedSource(t *testing.T) {
	packets := []packetRef{
		{DocID: "a-chunk1", FixtureID: "doc-a"},
		{DocID: "a-chunk2", FixtureID: "doc-a"},
		{DocID: "a-chunk3", FixtureID: "doc-a"},
	}
	expected := []ExpectedSource{{FixtureID: "doc-a"}}

	got := SourceRecallAtK(packets, expected, 3)
	if got != 1.0 {
		t.Errorf("SourceRecallAtK() = %v, want 1.0 (a repeated source must not exceed 1.0)", got)
	}
}

func TestReciprocalRankFirst(t *testing.T) {
	packets := []packetRef{
		{DocID: "a", FixtureID: "doc-a"},
		{DocID: "b", FixtureID: "doc-b"},
		{DocID: "c", FixtureID: "doc-c"},
	}

	tests := []struct {
		name     string
		expected []ExpectedSource
		want     float64
	}{
		{
			name:     "first position",
			expected: []ExpectedSource{{FixtureID: "doc-a"}},
			want:     1.0,
		},
		{
			name:     "second position",
			expected: []ExpectedSource{{FixtureID: "doc-b"}},
			want:     0.5,
		},
		{
			name:     "third position",
			expected: []ExpectedSource{{FixtureID: "doc-c"}},
			want:     1.0 / 3.0,
		},
		{
			name:     "not found",
			expected: []ExpectedSource{{FixtureID: "doc-x"}},
			want:     0.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ReciprocalRankFirst(packets, tt.expected)
			if got != tt.want {
				t.Errorf("ReciprocalRankFirst() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNDCG(t *testing.T) {
	packets := []packetRef{
		{DocID: "a", FixtureID: "doc-a"},
		{DocID: "b", FixtureID: "doc-b"},
	}

	tests := []struct {
		name     string
		expected []ExpectedSource
		grades   map[string]int
		wantMin  float64
		wantMax  float64
	}{
		{
			name:     "perfect ranking",
			expected: []ExpectedSource{{FixtureID: "doc-a", Grade: 3}, {FixtureID: "doc-b", Grade: 1}},
			grades:   map[string]int{"doc-a": 3, "doc-b": 1},
			wantMin:  0.9,
			wantMax:  1.1,
		},
		{
			name:     "binary relevance",
			expected: []ExpectedSource{{FixtureID: "doc-a"}, {FixtureID: "doc-b"}},
			grades:   nil,
			wantMin:  0.9,
			wantMax:  1.1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NDCG(packets, tt.expected, tt.grades)
			if got < tt.wantMin || got > tt.wantMax {
				t.Errorf("NDCG() = %v, want [%v, %v]", got, tt.wantMin, tt.wantMax)
			}
		})
	}
}

// TestNDCGDedupesRepeatedSource guards against a real bug: crediting a
// source's relevance grade on every chunk that mentions it, rather than
// once on its best-ranked occurrence, let achieved DCG exceed ideal DCG.
func TestNDCGDedupesRepeatedSource(t *testing.T) {
	packets := []packetRef{
		{DocID: "a-chunk1", FixtureID: "doc-a"},
		{DocID: "a-chunk2", FixtureID: "doc-a"},
		{DocID: "a-chunk3", FixtureID: "doc-a"},
	}
	expected := []ExpectedSource{{FixtureID: "doc-a", Grade: 3}}
	grades := map[string]int{"doc-a": 3}

	got := NDCG(packets, expected, grades)
	if got > 1.0+1e-9 {
		t.Errorf("NDCG() = %v, want <= 1.0 (a repeated source must not exceed ideal DCG)", got)
	}
	if got < 1.0-1e-9 {
		t.Errorf("NDCG() = %v, want 1.0 (the source is found at rank 1)", got)
	}
}

func TestTopicGroupCoverage(t *testing.T) {
	packets := []packetRef{
		{DocID: "a", FixtureID: "doc-a"},
		{DocID: "b", FixtureID: "doc-b"},
	}

	groups := []TopicGroup{
		{GroupID: "g1", FixtureIDs: []string{"doc-a"}},
		{GroupID: "g2", FixtureIDs: []string{"doc-b"}},
		{GroupID: "g3", FixtureIDs: []string{"doc-c"}},
	}

	got := TopicGroupCoverage(packets, groups)
	want := 2.0 / 3.0
	if got != want {
		t.Errorf("TopicGroupCoverage() = %v, want %v", got, want)
	}
}

func TestForbiddenHits(t *testing.T) {
	packets := []packetRef{
		{DocID: "a", FixtureID: "doc-a"},
		{DocID: "b", FixtureID: "doc-b"},
	}

	forbidden := []string{"doc-b", "doc-c"}

	got := ForbiddenHits(packets, forbidden)
	want := 1
	if got != want {
		t.Errorf("ForbiddenHits() = %v, want %v", got, want)
	}
}

func TestDistributionFrom(t *testing.T) {
	vals := []float64{10, 20, 30, 40, 50}
	got := DistributionFrom(vals)

	if got.N != 5 {
		t.Errorf("Distribution.N = %v, want 5", got.N)
	}
	if got.Min != 10 {
		t.Errorf("Distribution.Min = %v, want 10", got.Min)
	}
	if got.Max != 50 {
		t.Errorf("Distribution.Max = %v, want 50", got.Max)
	}
	if got.Mean != 30 {
		t.Errorf("Distribution.Mean = %v, want 30", got.Mean)
	}
}

func TestValidateCaseSet(t *testing.T) {
	tests := []struct {
		name    string
		cs      *CaseSet
		wantErr bool
	}{
		{
			name: "valid",
			cs: &CaseSet{
				Version: 1,
				SetID:   "test",
				Cases:   []Case{{ID: "c1", Question: "test?"}},
			},
			wantErr: false,
		},
		{
			name: "wrong version",
			cs: &CaseSet{
				Version: 2,
				SetID:   "test",
				Cases:   []Case{{ID: "c1", Question: "test?"}},
			},
			wantErr: true,
		},
		{
			name: "empty set_id",
			cs: &CaseSet{
				Version: 1,
				Cases:   []Case{{ID: "c1", Question: "test?"}},
			},
			wantErr: true,
		},
		{
			name: "no cases",
			cs: &CaseSet{
				Version: 1,
				SetID:   "test",
			},
			wantErr: true,
		},
		{
			name: "duplicate id",
			cs: &CaseSet{
				Version: 1,
				SetID:   "test",
				Cases: []Case{
					{ID: "c1", Question: "test?"},
					{ID: "c1", Question: "test2?"},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCaseSet(tt.cs)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateCaseSet() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestFilterCases(t *testing.T) {
	cases := []Case{
		{ID: "c1", Category: "exact-identifier"},
		{ID: "c2", Category: "paraphrase"},
		{ID: "c3", Category: "exact-identifier"},
		{ID: "c4", Category: "bilingual"},
	}

	t.Run("no filters", func(t *testing.T) {
		got := FilterCases(cases, nil, nil)
		if len(got) != 4 {
			t.Errorf("got %d cases, want 4", len(got))
		}
	})

	t.Run("filter by id", func(t *testing.T) {
		got := FilterCases(cases, []string{"c1", "c3"}, nil)
		if len(got) != 2 {
			t.Errorf("got %d cases, want 2", len(got))
		}
	})

	t.Run("filter by category", func(t *testing.T) {
		got := FilterCases(cases, nil, []string{"exact-identifier"})
		if len(got) != 2 {
			t.Errorf("got %d cases, want 2", len(got))
		}
	})

	t.Run("filter by both", func(t *testing.T) {
		got := FilterCases(cases, []string{"c1"}, []string{"exact-identifier"})
		if len(got) != 1 {
			t.Errorf("got %d cases, want 1", len(got))
		}
	})
}
