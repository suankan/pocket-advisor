package doctor

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestHealthyReport(t *testing.T) {
	d := New("test-ws", Checks{
		RegistryOK:    true,
		CredsOK:       true,
		PGRaw:         true,
		SourcesRaw:    true,
		EmbedModelOK:  true,
		EmbedDimOK:    true,
		PGVectorExtOK: true,
		PGSchemaOK:    true,
		PGHNSWOK:      true,
		PGBM25OK:      true,
	})
	r := d.Run(nil)
	if !r.Healthy {
		t.Errorf("expected healthy, got %d findings", len(r.Findings))
	}
	if r.ExitCode() != 0 {
		t.Errorf("exit code %d, want 0", r.ExitCode())
	}
}

func TestUnhealthyReport(t *testing.T) {
	d := New("test-ws", Checks{
		RegistryOK: true,
		CredsOK:    true,
		PGRaw:      false, // broken
		SourcesRaw: true,
		PGSchemaOK: true,
		PGHNSWOK:   true,
		PGBM25OK:   true,
	})
	r := d.Run(nil)
	if r.Healthy {
		t.Error("expected unhealthy")
	}
	if r.ExitCode() != 1 {
		t.Errorf("exit code %d, want 1", r.ExitCode())
	}
	found := false
	for _, f := range r.Findings {
		if f.Code == "PG_UNREACHABLE" {
			found = true
			if f.Severity != SeverityCritical {
				t.Errorf("PG_UNREACHABLE severity %q, want critical", f.Severity)
			}
		}
	}
	if !found {
		t.Error("missing PG_UNREACHABLE finding")
	}
}

func TestStalePendingFinding(t *testing.T) {
	d := New("test-ws", Checks{
		RegistryOK:   true,
		CredsOK:      true,
		PGRaw:        true,
		SourcesRaw:   true,
		PGSchemaOK:   true,
		PGHNSWOK:     true,
		PGBM25OK:     true,
		StalePending: 5,
	})
	r := d.Run(nil)
	if r.Healthy {
		t.Error("expected unhealthy with stale pending")
	}
	var found bool
	for _, f := range r.Findings {
		if f.Code == "STALE_PENDING" {
			found = true
			if f.Count != 5 {
				t.Errorf("count %d, want 5", f.Count)
			}
			if f.Severity != SeverityWarning {
				t.Errorf("severity %q, want warning", f.Severity)
			}
		}
	}
	if !found {
		t.Error("missing STALE_PENDING finding")
	}
}

func TestFailedByReasonFinding(t *testing.T) {
	d := New("test-ws", Checks{
		RegistryOK: true,
		CredsOK:    true,
		PGRaw:      true,
		SourcesRaw: true,
		PGSchemaOK: true,
		PGHNSWOK:   true,
		PGBM25OK:   true,
		FailedByReason: map[string]int{
			"EXTRACTION_FAILED": 3,
			"OCR_FAILED":        1,
			"UNKNOWN_ENCODING":  2,
		},
	})
	r := d.Run(nil)
	if r.Healthy {
		t.Error("expected unhealthy")
	}
	codes := map[string]bool{}
	for _, f := range r.Findings {
		codes[f.Code] = true
	}
	if !codes["FAILED_REASON_EXTRACTION_FAILED"] {
		t.Error("missing EXTRACTION_FAILED finding")
	}
	if !codes["FAILED_REASON_OCR_FAILED"] {
		t.Error("missing OCR_FAILED finding")
	}
	if !codes["FAILED_REASON_UNKNOWN_ENCODING"] {
		t.Error("missing UNKNOWN_ENCODING finding")
	}
}

// A failure or a password skip names its documents, and its advice says what
// to do with them: failures recorded before the job queue are for --recover,
// password-protected documents for the owner. Other skips are only counted.
func TestReasonFindingsNameTheirDocuments(t *testing.T) {
	d := New("test-ws", Checks{
		RegistryOK: true, CredsOK: true, PGRaw: true, SourcesRaw: true,
		PGSchemaOK: true, PGHNSWOK: true, PGBM25OK: true,
		FailedByReason:  map[string]int{"UNCLASSIFIED": 3, "UNKNOWN_ENCODING": 4},
		SkippedByReason: map[string]int{"PDF_ENCRYPTED": 1, "IMAGE_NOT_VIABLE": 9},
		FailedDocs: map[string]NamedDocs{
			"UNCLASSIFIED":     {Jobless: 3, Locations: []string{"mail/a.eml", "mail/b.eml"}},
			"UNKNOWN_ENCODING": {Jobless: 1, Locations: []string{"mail/c.eml"}},
		},
		LockedDocs: map[string]NamedDocs{"PDF_ENCRYPTED": {Locations: []string{"letter.pdf in mail/d.eml"}}},
	})
	findings := map[string]Finding{}
	for _, f := range d.Run(nil).Findings {
		findings[f.Code] = f
	}
	for code, want := range map[string]struct {
		docs   int
		action string
	}{
		"FAILED_REASON_UNCLASSIFIED":      {2, "recorded before the job queue — run --recover"},
		"FAILED_REASON_UNKNOWN_ENCODING":  {1, "1 recorded before the job queue, which --recover reruns; the rest: terminal"},
		"SKIPPED_REASON_PDF_ENCRYPTED":    {1, "password-protected"},
		"SKIPPED_REASON_IMAGE_NOT_VIABLE": {0, "expected decline"},
	} {
		f := findings[code]
		if len(f.Documents) != want.docs || !strings.HasPrefix(f.NextAction, want.action) {
			t.Errorf("%s: %d documents, action %q; want %d and %q", code, len(f.Documents), f.NextAction, want.docs, want.action)
		}
	}
}

// The unsupported-format finding is broken down by type, so the report says
// which formats the corpus holds that no handler reads.
func TestUnsupportedFindingGroupsByType(t *testing.T) {
	groups := []TypeGroup{
		{MimeType: "application/x-ole-storage", Extension: ".doc", Count: 2, Documents: []string{"docs/old.doc"}},
		{MimeType: "audio/mpeg", Extension: ".mp3", Count: 1},
	}
	d := New("test-ws", Checks{
		RegistryOK: true, CredsOK: true, PGRaw: true, SourcesRaw: true,
		PGSchemaOK: true, PGHNSWOK: true, PGBM25OK: true,
		SkippedByReason: map[string]int{"UNSUPPORTED_FORMAT": 3, "IMAGE_NOT_VIABLE": 9},
		Unsupported:     groups,
	})
	for _, f := range d.Run(nil).Findings {
		switch f.Code {
		case "SKIPPED_REASON_UNSUPPORTED_FORMAT":
			if len(f.Types) != 2 || f.Types[0].Extension != ".doc" || !strings.HasPrefix(f.NextAction, "no handler reads these formats yet") {
				t.Errorf("unsupported finding: types %+v, action %q", f.Types, f.NextAction)
			}
		case "SKIPPED_REASON_IMAGE_NOT_VIABLE":
			if len(f.Types) != 0 {
				t.Errorf("another skip carries types %+v", f.Types)
			}
		}
	}
}

func TestReportJSON(t *testing.T) {
	r := &Report{
		WorkspaceID: "test-ws",
		Healthy:     true,
		Findings:    []Finding{},
	}
	b, err := r.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var parsed Report
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.WorkspaceID != "test-ws" {
		t.Errorf("workspace_id %q, want test-ws", parsed.WorkspaceID)
	}
}

func TestZeroCountFindingsAreOmitted(t *testing.T) {
	d := New("test-ws", Checks{
		RegistryOK:    true,
		CredsOK:       true,
		PGRaw:         true,
		SourcesRaw:    true,
		EmbedModelOK:  true,
		EmbedDimOK:    true,
		PGVectorExtOK: true,
		PGSchemaOK:    true,
		PGHNSWOK:      true,
		PGBM25OK:      true,
		StalePending:  0, // zero count should not produce a finding
	})
	r := d.Run(nil)
	if !r.Healthy {
		for _, f := range r.Findings {
			t.Errorf("unexpected finding: %s (count=%d)", f.Code, f.Count)
		}
	}
}

func TestNilReportExitCode(t *testing.T) {
	var r *Report
	if r.ExitCode() != 2 {
		t.Errorf("nil report exit code %d, want 2", r.ExitCode())
	}
}

func TestIncompleteResetFinding(t *testing.T) {
	d := New("test-ws", Checks{
		RegistryOK:      true,
		CredsOK:         true,
		PGRaw:           true,
		SourcesRaw:      true,
		PGSchemaOK:      true,
		PGHNSWOK:        true,
		PGBM25OK:        true,
		IncompleteReset: true,
	})
	r := d.Run(nil)
	if r.Healthy {
		t.Error("expected unhealthy with incomplete reset")
	}
	found := false
	for _, f := range r.Findings {
		if f.Code == "INCOMPLETE_RESET" {
			found = true
		}
	}
	if !found {
		t.Error("missing INCOMPLETE_RESET finding")
	}
}

func TestSkippedByReasonFinding(t *testing.T) {
	d := New("test-ws", Checks{
		RegistryOK: true,
		CredsOK:    true,
		PGRaw:      true,
		SourcesRaw: true,
		PGSchemaOK: true,
		PGHNSWOK:   true,
		PGBM25OK:   true,
		SkippedByReason: map[string]int{
			"UNSUPPORTED_FORMAT": 10,
		},
	})
	r := d.Run(nil)
	// Skipped findings are info severity — check they are present
	found := false
	for _, f := range r.Findings {
		if f.Code == "SKIPPED_REASON_UNSUPPORTED_FORMAT" {
			found = true
			if f.Severity != SeverityInfo {
				t.Errorf("severity %q, want info", f.Severity)
			}
		}
	}
	if !found {
		t.Error("missing SKIPPED_REASON_UNSUPPORTED_FORMAT finding")
	}
}

func healthyChecks() Checks {
	return Checks{
		RegistryOK: true, CredsOK: true, PGRaw: true, SourcesRaw: true,
		EmbedModelOK: true, EmbedDimOK: true,
		PGVectorExtOK: true, PGSchemaOK: true, PGHNSWOK: true, PGBM25OK: true,
		QueueSchemaChecked: true, QueueSchemaReady: true,
	}
}

func findingCodes(r *Report) map[string]Finding {
	out := map[string]Finding{}
	for _, f := range r.Findings {
		out[f.Code] = f
	}
	return out
}

func TestOldQueueLayoutIsReportedAndNothingElseAboutJobs(t *testing.T) {
	c := healthyChecks()
	c.QueueSchemaReady = false
	// Stale values from a layout that cannot be read must not leak into findings.
	r := New("ws", c).Run(nil)
	f, ok := findingCodes(r)["QUEUE_SCHEMA_UPGRADE_REQUIRED"]
	if !ok {
		t.Fatal("missing QUEUE_SCHEMA_UPGRADE_REQUIRED")
	}
	if f.Severity != SeverityError || f.NextAction == "" {
		t.Errorf("finding = %+v, want an error with a next action", f)
	}
	if r.Healthy {
		t.Error("an old queue layout reported healthy")
	}
}

func TestMissingSchemaDoesNotAlsoReportAnUpgrade(t *testing.T) {
	c := healthyChecks()
	c.PGSchemaOK, c.QueueSchemaReady = false, false
	if _, ok := findingCodes(New("ws", c).Run(nil))["QUEUE_SCHEMA_UPGRADE_REQUIRED"]; ok {
		t.Error("a database with no schema was told to upgrade a queue it does not have")
	}
}

func TestQueueFindingsCarrySeverityCountAndAction(t *testing.T) {
	c := healthyChecks()
	c.ExpiredLeases = 3
	c.ExhaustedBudgets = 2
	c.FailedJobsByReason = map[string]int{"OCR_FAILED": 4, "MALFORMED_COMMAND": 1}
	c.JobsByState = map[string]int{"READY": 5, "RUNNING": 2, "FAILED": 5}
	r := New("ws", c).Run(nil)
	codes := findingCodes(r)
	for code, want := range map[string]struct {
		sev   Severity
		count int
	}{
		"JOBS_EXPIRED_LEASES":                  {SeverityWarning, 3},
		"JOBS_EXHAUSTED_BUDGET":                {SeverityError, 2},
		"JOBS_FAILED_REASON_OCR_FAILED":        {SeverityError, 4},
		"JOBS_FAILED_REASON_MALFORMED_COMMAND": {SeverityError, 1},
	} {
		f, ok := codes[code]
		if !ok {
			t.Errorf("missing %s", code)
			continue
		}
		if f.Severity != want.sev || f.Count != want.count || f.Summary == "" || f.NextAction == "" {
			t.Errorf("%s = %+v", code, f)
		}
	}
	if !strings.Contains(codes["JOBS_FAILED_REASON_OCR_FAILED"].NextAction, "--recover") {
		t.Errorf("a retryable failure should point at --recover: %q", codes["JOBS_FAILED_REASON_OCR_FAILED"].NextAction)
	}
	if r.Queue["RUNNING"] != 2 || r.Queue["READY"] != 5 {
		t.Errorf("queue summary = %v, want the per-state counts", r.Queue)
	}
}

// Live RUNNING work is a count in the summary, never a finding, so an active
// ingestion does not make a workspace look unhealthy.
func TestLiveRunningWorkIsNotAFinding(t *testing.T) {
	c := healthyChecks()
	c.JobsByState = map[string]int{"RUNNING": 12, "READY": 40}
	r := New("ws", c).Run(nil)
	if !r.Healthy {
		t.Fatalf("live work produced findings: %+v", r.Findings)
	}
	if r.Queue["RUNNING"] != 12 {
		t.Errorf("queue summary = %v", r.Queue)
	}
}

func TestReportContainsNoBrokerFindings(t *testing.T) {
	c := healthyChecks()
	c.PGRaw = false
	for code := range findingCodes(New("ws", c).Run(nil)) {
		if strings.HasPrefix(code, "NATS_") || strings.HasPrefix(code, "JS_") {
			t.Errorf("broker finding %s survived the migration", code)
		}
	}
}

func TestScopeMismatchReportsOnlyTheMismatch(t *testing.T) {
	c := healthyChecks()
	c.ScopeMismatch = true
	c.PGSchemaOK, c.PGVectorExtOK, c.PGHNSWOK, c.PGBM25OK = false, false, false, false
	r := New("ws", c).Run(nil)
	codes := findingCodes(r)
	if f, ok := codes["WORKSPACE_SCOPE_MISMATCH"]; !ok || f.Severity != SeverityCritical {
		t.Fatalf("missing a critical WORKSPACE_SCOPE_MISMATCH: %+v", r.Findings)
	}
	if len(r.Findings) != 1 || r.Healthy {
		t.Errorf("findings = %+v, want only the mismatch: nothing else may describe another workspace's database", r.Findings)
	}
}
