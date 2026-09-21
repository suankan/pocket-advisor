package eval

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func report(runID, setSHA string, at time.Time, cases ...CaseReport) *Report {
	r := &Report{RunID: runID, CaseSetSHA256: setSHA, Timestamp: at, Cases: cases}
	for _, c := range cases {
		if c.Passed {
			r.Summary.PassedCases++
		}
	}
	r.Summary.TotalCases = len(cases)
	return r
}

func TestArchiveRunWritesAndReloads(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 9, 11, 8, 30, 12, 0, time.UTC)
	in := report("eval-20260911T083012Z", "sha-1", at, CaseReport{CaseID: "c1", Passed: true})

	path, err := ArchiveRun(dir, in)
	if err != nil {
		t.Fatalf("ArchiveRun() error = %v", err)
	}
	if got := filepath.Base(path); got != "eval-20260911T083012Z.json" {
		t.Errorf("file = %q, want the run id so the directory sorts chronologically", got)
	}
	back, err := LoadReport(path)
	if err != nil {
		t.Fatalf("LoadReport() error = %v", err)
	}
	if back.RunID != in.RunID || back.Summary.PassedCases != 1 {
		t.Errorf("reloaded = %+v, want the archived run", back.Summary)
	}
}

// Two runs in the same second must both survive; a measurement already taken
// is not something a later one gets to erase.
func TestArchiveRunDoesNotOverwriteSameSecondRun(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 9, 11, 8, 30, 12, 0, time.UTC)
	first, err := ArchiveRun(dir, report("eval-x", "sha-1", at))
	if err != nil {
		t.Fatal(err)
	}
	second, err := ArchiveRun(dir, report("eval-x", "sha-1", at))
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("second run overwrote the first")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("archived %d files, want 2", len(entries))
	}
}

func TestPreviousComparableRunSkipsOtherCaseSets(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC)
	mustArchive(t, dir, report("eval-20260911T080000Z", "sha-1", base))
	mustArchive(t, dir, report("eval-20260911T090000Z", "sha-other", base.Add(time.Hour)))

	cur := report("eval-20260911T100000Z", "sha-1", base.Add(2*time.Hour))
	prev, err := PreviousComparableRun(dir, cur)
	if err != nil {
		t.Fatalf("PreviousComparableRun() error = %v", err)
	}
	if prev == nil || prev.RunID != "eval-20260911T080000Z" {
		t.Errorf("previous = %v, want the most recent run over the same case set", prev)
	}
}

func TestPreviousComparableRunIgnoresTheCurrentAndLaterRuns(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC)
	cur := report("eval-20260911T080000Z", "sha-1", base)
	mustArchive(t, dir, cur)
	mustArchive(t, dir, report("eval-20260911T090000Z", "sha-1", base.Add(time.Hour)))

	prev, err := PreviousComparableRun(dir, cur)
	if err != nil {
		t.Fatalf("PreviousComparableRun() error = %v", err)
	}
	if prev != nil {
		t.Errorf("previous = %v, want none — nothing earlier exists", prev.RunID)
	}
}

func TestPreviousComparableRunHandlesAnEmptyOrMissingDirectory(t *testing.T) {
	cur := report("eval-1", "sha-1", time.Now().UTC())
	prev, err := PreviousComparableRun(filepath.Join(t.TempDir(), "never-created"), cur)
	if err != nil || prev != nil {
		t.Errorf("previous = %v, err = %v, want no comparison and no error", prev, err)
	}
}

// A corrupt archive must not stop the current run from finding an older
// comparable one behind it.
func TestPreviousComparableRunSkipsUnreadableArchives(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC)
	mustArchive(t, dir, report("eval-20260911T080000Z", "sha-1", base))
	if err := os.WriteFile(filepath.Join(dir, "eval-20260911T090000Z.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	cur := report("eval-20260911T100000Z", "sha-1", base.Add(2*time.Hour))
	prev, err := PreviousComparableRun(dir, cur)
	if err != nil {
		t.Fatalf("PreviousComparableRun() error = %v", err)
	}
	if prev == nil || prev.RunID != "eval-20260911T080000Z" {
		t.Errorf("previous = %v, want the readable earlier run", prev)
	}
}

func TestCompareReportsMovementAndTheCasesBehindIt(t *testing.T) {
	at := time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC)
	prev := report("eval-prev", "sha-1", at,
		CaseReport{CaseID: "a", Passed: true},
		CaseReport{CaseID: "b", Passed: false},
		CaseReport{CaseID: "c", Passed: true})
	prev.Summary.MeanRecall, prev.Summary.MeanDeliveredRecall = 0.80, 0.70
	cur := report("eval-cur", "sha-1", at.Add(time.Hour),
		CaseReport{CaseID: "a", Passed: false},
		CaseReport{CaseID: "b", Passed: true},
		CaseReport{CaseID: "c", Passed: true})
	cur.Summary.MeanRecall, cur.Summary.MeanDeliveredRecall = 0.85, 0.90

	d := Compare(prev, cur)
	// An equal number of fixes and regressions leaves every mean looking
	// calm, which is exactly why the case lists exist.
	if d.PassedCases != 0 {
		t.Errorf("PassedCases = %d, want 0", d.PassedCases)
	}
	if len(d.Fixed) != 1 || d.Fixed[0] != "b" {
		t.Errorf("Fixed = %v, want [b]", d.Fixed)
	}
	if len(d.Regressed) != 1 || d.Regressed[0] != "a" {
		t.Errorf("Regressed = %v, want [a]", d.Regressed)
	}
	if d.MeanRecall < 0.049 || d.MeanRecall > 0.051 {
		t.Errorf("MeanRecall = %v, want about +0.05", d.MeanRecall)
	}
	if d.MeanDeliveredRecall < 0.199 || d.MeanDeliveredRecall > 0.201 {
		t.Errorf("MeanDeliveredRecall = %v, want about +0.20", d.MeanDeliveredRecall)
	}
}

// A case added or removed between runs has nothing to compare against and
// must not be reported as either a fix or a regression.
func TestCompareIgnoresCasesAbsentFromTheEarlierRun(t *testing.T) {
	at := time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC)
	prev := report("eval-prev", "sha-1", at, CaseReport{CaseID: "a", Passed: true})
	cur := report("eval-cur", "sha-1", at.Add(time.Hour),
		CaseReport{CaseID: "a", Passed: true},
		CaseReport{CaseID: "new", Passed: false})

	d := Compare(prev, cur)
	if len(d.Regressed) != 0 || len(d.Fixed) != 0 {
		t.Errorf("fixed = %v, regressed = %v, want neither for an unmatched case", d.Fixed, d.Regressed)
	}
}

func mustArchive(t *testing.T, dir string, r *Report) {
	t.Helper()
	if _, err := ArchiveRun(dir, r); err != nil {
		t.Fatal(err)
	}
}
