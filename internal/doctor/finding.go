// Package doctor provides transport-independent read-only health checks
// and recovery planning for a single workspace. The CLI adapter renders
// findings; the core package never touches stdout.
package doctor

import "encoding/json"

// Severity is the finding's urgency.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityError    Severity = "error"
	SeverityCritical Severity = "critical"
)

// Finding is one diagnostic result. Every finding carries a stable code
// that monitoring and scripting can filter on, a severity, an affected
// count, a safe summary that must not contain content, filenames, or
// credentials, and a next action the operator can take.
type Finding struct {
	Code       string   `json:"code"`
	Severity   Severity `json:"severity"`
	Count      int      `json:"count"`
	Summary    string   `json:"summary"`
	NextAction string   `json:"next_action"`
	// Documents names the documents behind the finding by location, the most
	// recently updated first. It may name fewer than Count.
	Documents []string `json:"documents,omitempty"`
	// Types breaks the finding down by detected type and file extension, the
	// largest group first.
	Types []TypeGroup `json:"types,omitempty"`
}

// TypeGroup is the documents of one detected type and file extension, named
// as Documents are.
type TypeGroup struct {
	MimeType  string   `json:"mime_type"`
	Extension string   `json:"extension"`
	Count     int      `json:"count"`
	Documents []string `json:"documents,omitempty"`
}

// Report is the complete output of a doctor run.
type Report struct {
	WorkspaceID string    `json:"workspace_id"`
	Healthy     bool      `json:"healthy"`
	Findings    []Finding `json:"findings"`
	// Queue counts jobs per state. Live RUNNING work appears here and is not
	// a finding: a job under an unexpired lease is working, however old its
	// document row is.
	Queue map[string]int `json:"queue,omitempty"`
}

// JSON serialises the report.
func (r *Report) JSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// ExitCode returns the CLI exit code. 0 = healthy, 1 = unhealthy but
// diagnosed, 2 = doctor execution failure. A report is healthy when no
// finding is above info.
func (r *Report) ExitCode() int {
	if r == nil {
		return 2
	}
	if r.Healthy {
		return 0
	}
	return 1
}

func addFinding(findings *[]Finding, code string, sev Severity, count int, summary, action string) {
	if count == 0 {
		return
	}
	*findings = append(*findings, Finding{
		Code:       code,
		Severity:   sev,
		Count:      count,
		Summary:    summary,
		NextAction: action,
	})
}
