package eval

import (
	"testing"
	"time"

	"github.com/suankan/pocket-advisor/internal/retrieval"
)

func TestLatencyReportUsesStageResultDurations(t *testing.T) {
	report := latencyReport([]*retrieval.StageResult{
		{
			EmbedDuration:   time.Millisecond,
			DenseDuration:   2 * time.Millisecond,
			LexicalDuration: 3 * time.Millisecond,
			FuseDuration:    4 * time.Millisecond,
			RerankDuration:  5 * time.Millisecond,
			SelectDuration:  6 * time.Millisecond,
			ExpandDuration:  7 * time.Millisecond,
			TotalDuration:   8 * time.Millisecond,
		},
		{
			EmbedDuration:   3 * time.Millisecond,
			DenseDuration:   4 * time.Millisecond,
			LexicalDuration: 5 * time.Millisecond,
			FuseDuration:    6 * time.Millisecond,
			RerankDuration:  7 * time.Millisecond,
			SelectDuration:  8 * time.Millisecond,
			ExpandDuration:  9 * time.Millisecond,
			TotalDuration:   10 * time.Millisecond,
		},
	})

	for name, distribution := range map[string]Distribution{
		"embed": report.EmbedMS, "dense": report.DenseMS,
		"lexical": report.LexicalMS, "fused": report.FusedMS,
		"rerank": report.RerankMS, "select": report.SelectMS,
		"expand": report.ExpandMS, "total": report.TotalMS,
	} {
		if distribution.N != 2 {
			t.Errorf("%s distribution N = %d, want 2", name, distribution.N)
		}
	}
	if got, want := report.FusedMS.Mean, 5.0; got != want {
		t.Errorf("fused mean = %v, want %v", got, want)
	}
	if got, want := report.TotalMS.P95, 10.0; got != want {
		t.Errorf("total p95 = %v, want %v", got, want)
	}
}

func TestLatencyReportOmitsMissingLegDurations(t *testing.T) {
	report := latencyReport([]*retrieval.StageResult{{
		EmbedDuration: time.Millisecond,
		FuseDuration:  2 * time.Millisecond,
		TotalDuration: 3 * time.Millisecond,
	}})

	if report.DenseMS.N != 0 || report.LexicalMS.N != 0 {
		t.Errorf("dense/lexical observations = %d/%d, want 0/0", report.DenseMS.N, report.LexicalMS.N)
	}
	if report.FusedMS.N != 1 || report.FusedMS.Mean != 2 {
		t.Errorf("fused distribution = %+v, want one 2 ms observation", report.FusedMS)
	}
}

func TestLatencyReportEmpty(t *testing.T) {
	if got := latencyReport(nil).TotalMS.N; got != 0 {
		t.Errorf("empty total distribution N = %d, want 0", got)
	}
}
