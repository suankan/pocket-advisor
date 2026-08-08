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
}

// Report is the complete output of a doctor run.
type Report struct {
	WorkspaceID string    `json:"workspace_id"`
	Healthy     bool      `json:"healthy"`
	Findings    []Finding `json:"findings"`
}

// JSON serialises the report.
func (r *Report) JSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// ExitCode returns the CLI exit code. 0 = healthy, 1 = unhealthy but
// diagnosed, 2 = doctor execution failure.
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
