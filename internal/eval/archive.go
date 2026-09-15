package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ArchiveRun writes a report into a run directory and returns its path.
//
// The file name is the run id, which is a UTC timestamp, so the directory
// sorts chronologically by name alone and needs no index. A second run
// within the same second gets a numbered suffix rather than overwriting the
// first: a measurement that has been taken should not disappear because
// another one happened to land beside it.
func ArchiveRun(dir string, r *Report) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create run directory: %w", err)
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode report: %w", err)
	}
	for attempt := 1; ; attempt++ {
		name := r.RunID + ".json"
		if attempt > 1 {
			name = fmt.Sprintf("%s-%d.json", r.RunID, attempt)
		}
		path := filepath.Join(dir, name)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("create report file: %w", err)
		}
		_, writeErr := f.Write(append(raw, '\n'))
		closeErr := f.Close()
		if writeErr != nil {
			return "", fmt.Errorf("write report: %w", writeErr)
		}
		if closeErr != nil {
			return "", fmt.Errorf("close report: %w", closeErr)
		}
		return path, nil
	}
}

// LoadReport reads one archived report.
func LoadReport(path string) (*Report, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("parse report %s: %w", path, err)
	}
	return &r, nil
}

// PreviousComparableRun returns the most recent archived run that measured
// the same questions as cur, or nil when there is none.
//
// Comparability is the case-set digest, not the file order. Two runs over
// different suites produce numbers that look like a trend and are not one,
// and silently plotting them together is a worse outcome than showing no
// comparison at all.
func PreviousComparableRun(dir string, cur *Report) (*Report, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))

	for _, name := range names {
		prev, err := LoadReport(filepath.Join(dir, name))
		if err != nil {
			// A corrupt or half-written archive must not stop the current run
			// from reporting. Skip it and keep looking further back.
			continue
		}
		if prev.RunID == cur.RunID || prev.Timestamp.After(cur.Timestamp) {
			continue
		}
		if prev.CaseSetSHA256 != cur.CaseSetSHA256 {
			continue
		}
		return prev, nil
	}
	return nil, nil
}

// Delta is the movement between two comparable runs.
//
// Aggregate movement says whether the system got better; the case lists say
// where. Both matter, and a mean alone can hide an equal number of fixes and
// regressions as no change at all.
type Delta struct {
	PreviousRunID string    `json:"previous_run_id"`
	PreviousAt    time.Time `json:"previous_at"`
	PreviousSHA   string    `json:"previous_commit_sha"`

	PassedCases    int     `json:"passed_cases"`
	MeanRecall     float64 `json:"mean_recall"`
	MeanMRR        float64 `json:"mean_mrr"`
	MeanNDCG       float64 `json:"mean_ndcg"`
	MeanFactRecall float64 `json:"mean_fact_recall"`
	MeanFactMRR    float64 `json:"mean_fact_mrr"`

	// Fixed and Regressed are case ids, which are the operator's own stable
	// labels rather than workspace content.
	Fixed     []string `json:"fixed,omitempty"`
	Regressed []string `json:"regressed,omitempty"`
}

// Compare reports how cur moved relative to prev.
func Compare(prev, cur *Report) Delta {
	d := Delta{
		PreviousRunID:  prev.RunID,
		PreviousAt:     prev.Timestamp,
		PreviousSHA:    prev.CommitSHA,
		PassedCases:    cur.Summary.PassedCases - prev.Summary.PassedCases,
		MeanRecall:     cur.Summary.MeanRecall - prev.Summary.MeanRecall,
		MeanMRR:        cur.Summary.MeanMRR - prev.Summary.MeanMRR,
		MeanNDCG:       cur.Summary.MeanNDCG - prev.Summary.MeanNDCG,
		MeanFactRecall: cur.Summary.MeanFactRecall - prev.Summary.MeanFactRecall,
		MeanFactMRR:    cur.Summary.MeanFactMRR - prev.Summary.MeanFactMRR,
	}

	was := make(map[string]bool, len(prev.Cases))
	for _, c := range prev.Cases {
		was[c.CaseID] = c.Passed
	}
	for _, c := range cur.Cases {
		passedBefore, known := was[c.CaseID]
		if !known {
			continue
		}
		switch {
		case c.Passed && !passedBefore:
			d.Fixed = append(d.Fixed, c.CaseID)
		case !c.Passed && passedBefore:
			d.Regressed = append(d.Regressed, c.CaseID)
		}
	}
	sort.Strings(d.Fixed)
	sort.Strings(d.Regressed)
	return d
}
