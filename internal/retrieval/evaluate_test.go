package retrieval

import (
	"testing"
	"time"
)

func TestStageResultRecordFusedCandidates(t *testing.T) {
	sr := &StageResult{}
	sr.recordFusedCandidates([][]candidate{{
		{ChunkID: "chunk-a", DocID: "doc-a", DenseRank: 1, RRF: 0.1},
		{ChunkID: "chunk-b", DocID: "doc-b", LexRank: 1, RRF: 0.2},
	}, {
		{ChunkID: "chunk-c", DocID: "doc-a", DenseRank: 2, LexRank: 2, RRF: 0.3},
	}})

	if got, want := sr.FusedCount, 3; got != want {
		t.Errorf("FusedCount = %d, want %d", got, want)
	}
	if got, want := len(sr.FusedCandidates), 3; got != want {
		t.Errorf("len(FusedCandidates) = %d, want %d", got, want)
	}
	if got, want := sr.FusedDocIDs(), []string{"doc-a", "doc-b"}; !equalStrings(got, want) {
		t.Errorf("FusedDocIDs() = %v, want %v", got, want)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestStageResultRecordFusionDuration(t *testing.T) {
	sr := &StageResult{}
	const duration = 12 * time.Millisecond
	sr.recordFusionDuration(duration)

	if sr.DenseDuration != duration || sr.LexicalDuration != duration || sr.FuseDuration != duration {
		t.Errorf("fusion durations = dense %v, lexical %v, fused %v; want %v each", sr.DenseDuration, sr.LexicalDuration, sr.FuseDuration, duration)
	}
}
