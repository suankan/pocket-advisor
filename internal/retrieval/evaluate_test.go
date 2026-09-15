package retrieval

import (
	"testing"
	"time"
)

// A nil observation is the production path. Every method has to tolerate it,
// because the alternative is a branch at each call site in the read path.
func TestNilRunObservationRecordsNothingAndDoesNotPanic(t *testing.T) {
	var obs *runObservation

	done := obs.begin(stageEmbed)
	done()
	obs.recordFused([][]candidate{{{ChunkID: "chunk-a", DocID: "doc-a"}}})
	obs.recordRanked([]scored{{}})
	obs.recordSelected(3)
}

func TestRunObservationProjectsStagesOntoTheResult(t *testing.T) {
	obs := &runObservation{}
	obs.recordFused([][]candidate{{
		{ChunkID: "chunk-a", DocID: "doc-a", DenseRank: 1, RRF: 0.1},
		{ChunkID: "chunk-b", DocID: "doc-b", LexRank: 1, RRF: 0.2},
	}, {
		{ChunkID: "chunk-c", DocID: "doc-a", DenseRank: 2, LexRank: 2, RRF: 0.3},
	}})
	obs.recordSelected(2)

	res := &Result{Question: "q", Packets: []Packet{{}}}
	sr := obs.stageResult(res)

	// Evaluation reads the production result itself, so nothing about it can
	// go unmeasured by omission.
	if sr.Result != res || sr.Question != "q" || len(sr.Packets) != 1 {
		t.Errorf("stage result did not carry the caller's own result: %+v", sr)
	}
	if sr.FusedCount != 3 || len(sr.FusedCandidates) != 3 {
		t.Errorf("FusedCount = %d, len = %d, want 3 and 3", sr.FusedCount, len(sr.FusedCandidates))
	}
	// Two candidates carry a dense rank and two a lexical rank; one carries
	// both, which must count once on each leg rather than once overall.
	if sr.DenseCount != 2 || sr.LexicalCount != 2 {
		t.Errorf("dense = %d, lexical = %d, want 2 and 2", sr.DenseCount, sr.LexicalCount)
	}
	if sr.SelectCount != 2 {
		t.Errorf("SelectCount = %d, want 2", sr.SelectCount)
	}
	if got, want := sr.FusedDocIDs(), []string{"doc-a", "doc-b"}; !equalStrings(got, want) {
		t.Errorf("FusedDocIDs() = %v, want %v", got, want)
	}
}

// Both legs are computed in one SQL statement, so their durations are the
// same measurement under three names rather than three additive costs.
func TestFusionDurationIsReportedUnderEveryLegItPerforms(t *testing.T) {
	obs := &runObservation{}
	done := obs.begin(stageFusion)
	time.Sleep(time.Millisecond)
	done()

	sr := obs.stageResult(&Result{})
	if sr.FuseDuration <= 0 {
		t.Fatalf("FuseDuration = %v, want a positive measurement", sr.FuseDuration)
	}
	if sr.DenseDuration != sr.FuseDuration || sr.LexicalDuration != sr.FuseDuration {
		t.Errorf("dense = %v, lexical = %v, want both equal to fuse = %v",
			sr.DenseDuration, sr.LexicalDuration, sr.FuseDuration)
	}
}

// Pooling and selection are separate calls that belong to one reported stage.
func TestSelectionDurationAccumulatesAcrossItsStages(t *testing.T) {
	obs := &runObservation{}
	for range 2 {
		done := obs.begin(stageSelection)
		time.Sleep(time.Millisecond)
		done()
	}
	sr := obs.stageResult(&Result{})
	if sr.SelectDuration < 2*time.Millisecond {
		t.Errorf("SelectDuration = %v, want both pooling and selection counted", sr.SelectDuration)
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
