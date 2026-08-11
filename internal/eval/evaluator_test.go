package eval

import (
	"strings"
	"testing"
)

func TestCheckCaseUsesExpectedDocumentContracts(t *testing.T) {
	e := &Evaluator{}
	tests := []struct {
		name       string
		c          Case
		cr         CaseReport
		wantPassed bool
		wantText   string
	}{
		{name: "expected empty accepts no packets", c: Case{ExpectedEmpty: true}, cr: CaseReport{}, wantPassed: true},
		{name: "expected empty records forbidden result", c: Case{ExpectedEmpty: true}, cr: CaseReport{PacketCount: 1, ForbiddenHits: 1}, wantText: "forbidden"},
		{
			name:     "regular case requires one expected document",
			c:        Case{ExpectedDocuments: []ExpectedDocument{{DocumentID: "wanted", Grade: 1}}},
			cr:       CaseReport{PacketCount: 1, DocumentRecallAtK: 0},
			wantText: "no expected document",
		},
		{
			name:       "one expected document is sufficient by default",
			c:          Case{ExpectedDocuments: []ExpectedDocument{{DocumentID: "a", Grade: 1}, {DocumentID: "b", Grade: 1}}},
			cr:         CaseReport{PacketCount: 1, DocumentRecallAtK: 0.5},
			wantPassed: true,
		},
		{
			name:     "require all expected documents rejects partial coverage",
			c:        Case{RequireAllExpectedDocuments: true},
			cr:       CaseReport{ExpectedDocumentCount: 2, DocumentRecallAtK: 0.5},
			wantText: "found 1 of 2",
		},
		{
			name:       "require all expected documents accepts full coverage",
			c:          Case{RequireAllExpectedDocuments: true},
			cr:         CaseReport{ExpectedDocumentCount: 2, DocumentRecallAtK: 1},
			wantPassed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failures := e.checkCase(tt.c, &tt.cr)
			gotPassed := len(failures) == 0
			if gotPassed != tt.wantPassed {
				t.Fatalf("passed = %v, failures = %v, want passed %v", gotPassed, failures, tt.wantPassed)
			}
			if tt.wantText != "" && !strings.Contains(strings.Join(failures, "; "), tt.wantText) {
				t.Errorf("failures = %v, want text %q", failures, tt.wantText)
			}
		})
	}
}

func TestBuildSummaryUsesApplicableMetricDenominators(t *testing.T) {
	e := &Evaluator{}
	cases := []CaseReport{
		{Passed: true, ExpectedDocumentCount: 2, DocumentRecallAtK: 0.8, RRFirstExpectedDocument: 0.8, NDCG: 0.8},
		{Passed: true, ExpectedEmpty: true, PacketCount: 0},
		{Passed: true, ExpectedDocumentCount: 1, DocumentRecallAtK: 0.8, RRFirstExpectedDocument: 0.8, NDCG: 0.8},
	}
	s := e.buildSummary(cases, EvaluateConfig{})
	if s.PassedCases != 3 || s.FailedCases != 0 || s.RankedCases != 2 {
		t.Errorf("summary = %+v, want three passed cases and two ranked", s)
	}
	if s.MeanRecall != 0.8 || s.MeanMRR != 0.8 || s.MeanNDCG != 0.8 || s.EmptyPassRate != 1 {
		t.Errorf("metric summary = %+v, want 0.8 ranking metrics and empty pass rate 1", s)
	}
	if !s.OverallPassed {
		t.Error("overall failed when all applicable thresholds were met")
	}
}

func TestApplyHNSWThresholdOnlyUsesComparisonResults(t *testing.T) {
	thresholds := DefaultThresholds()
	for _, tt := range []struct {
		name string
		hnsw *HNSWReport
		want bool
	}{
		{name: "not requested or unavailable", want: true},
		{name: "no comparable cases", hnsw: &HNSWReport{}, want: true},
		{name: "below threshold", hnsw: &HNSWReport{CasesCompared: 1, MeanRecall: 0.84}, want: false},
		{name: "at threshold", hnsw: &HNSWReport{CasesCompared: 1, MeanRecall: 0.85}, want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := Summary{OverallPassed: true}
			applyHNSWThreshold(&s, tt.hnsw, thresholds)
			if s.OverallPassed != tt.want {
				t.Errorf("overall passed = %v, want %v", s.OverallPassed, tt.want)
			}
		})
	}
}
