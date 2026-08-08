package doctor

import (
	"encoding/json"
	"testing"

)

func TestHealthyReport(t *testing.T) {
	d := New("test-ws", Checks{
		RegistryOK:  true,
		CredsOK:     true,
		PGRaw:       true,
		RustFSRaw:   true,
		NATSRaw:     true,
		EmbedModelOK: true,
		EmbedDimOK:  true,
		PGVectorExtOK: true,
		PGSchemaOK:  true,
		PGHNSWOK:    true,
		PGBM25OK:    true,
		StreamsOK:    true,
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
		RegistryOK:  true,
		CredsOK:     true,
		PGRaw:       false, // broken
		RustFSRaw:   true,
		NATSRaw:     true,
		PGSchemaOK:  true,
		PGHNSWOK:    true,
		PGBM25OK:    true,
		StreamsOK:    true,
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
		RustFSRaw:    true,
		NATSRaw:      true,
		PGSchemaOK:   true,
		PGHNSWOK:     true,
		PGBM25OK:     true,
		StreamsOK:     true,
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
		RegistryOK:   true,
		CredsOK:      true,
		PGRaw:        true,
		RustFSRaw:    true,
		NATSRaw:      true,
		PGSchemaOK:   true,
		PGHNSWOK:     true,
		PGBM25OK:     true,
		StreamsOK:     true,
		FailedByReason: map[string]int{
			"EXTRACTION_FAILED":  3,
			"OCR_FAILED":         1,
			"UNKNOWN_ENCODING":   2,
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
		RegistryOK:     true,
		CredsOK:        true,
		PGRaw:          true,
		RustFSRaw:      true,
		NATSRaw:        true,
		EmbedModelOK:   true,
		EmbedDimOK:     true,
		PGVectorExtOK:  true,
		PGSchemaOK:     true,
		PGHNSWOK:       true,
		PGBM25OK:       true,
		StreamsOK:       true,
		StalePending:   0, // zero count should not produce a finding
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
		RegistryOK:     true,
		CredsOK:        true,
		PGRaw:          true,
		RustFSRaw:      true,
		NATSRaw:        true,
		PGSchemaOK:     true,
		PGHNSWOK:       true,
		PGBM25OK:       true,
		StreamsOK:      true,
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
		RegistryOK:   true,
		CredsOK:      true,
		PGRaw:        true,
		RustFSRaw:    true,
		NATSRaw:      true,
		PGSchemaOK:   true,
		PGHNSWOK:     true,
		PGBM25OK:     true,
		StreamsOK:     true,
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
