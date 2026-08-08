package doctor

import (
	"testing"
)

// TestSyntheticScenarios verifies that each synthetic scenario produces
// deterministic doctor and recovery results. These serve as the
// foundation for fault-injection testing: each scenario represents a
// specific workspace state, and the assertions verify that doctor and
// recovery produce the expected output for that state.

func TestSyntheticHealthyScenario(t *testing.T) {
	checks := SyntheticChecks("healthy")
	d := New("synthetic", checks)
	r := d.Run(nil)
	if !r.Healthy {
		t.Errorf("healthy scenario has %d findings", len(r.Findings))
		for _, f := range r.Findings {
			t.Logf("  %s: %s", f.Code, f.Summary)
		}
	}
}

func TestSyntheticStalePendingScenario(t *testing.T) {
	checks := SyntheticChecks("stale_pending")
	d := New("synthetic", checks)
	r := d.Run(nil)
	if r.Healthy {
		t.Fatal("stale_pending scenario should be unhealthy")
	}
	found := false
	for _, f := range r.Findings {
		if f.Code == "STALE_PENDING" && f.Count == 12 {
			found = true
		}
	}
	if !found {
		t.Error("missing STALE_PENDING finding with count 12")
	}
}

func TestSyntheticStaleProcessingScenario(t *testing.T) {
	checks := SyntheticChecks("stale_processing")
	d := New("synthetic", checks)
	r := d.Run(nil)
	found := false
	for _, f := range r.Findings {
		if f.Code == "STALE_PROCESSING" && f.Count == 3 && f.Severity == SeverityError {
			found = true
		}
	}
	if !found {
		t.Error("missing STALE_PROCESSING finding with count 3 and error severity")
	}
}

func TestSyntheticFailedMixedScenario(t *testing.T) {
	checks := SyntheticChecks("failed_mixed")
	d := New("synthetic", checks)
	r := d.Run(nil)
	codes := map[string]bool{}
	for _, f := range r.Findings {
		codes[f.Code] = true
	}
	for _, code := range []string{
		"FAILED_REASON_EXTRACTION_FAILED",
		"FAILED_REASON_OCR_FAILED",
		"FAILED_REASON_UNKNOWN_ENCODING",
		"FAILED_REASON_MALFORMED_COMMAND",
		"SKIPPED_REASON_UNSUPPORTED_FORMAT",
		"SKIPPED_REASON_IMAGE_NOT_VIABLE",
	} {
		if !codes[code] {
			t.Errorf("missing finding: %s", code)
		}
	}
}

func TestSyntheticSchemaMissingScenario(t *testing.T) {
	checks := SyntheticChecks("schema_missing")
	d := New("synthetic", checks)
	r := d.Run(nil)
	criticalCount := 0
	for _, f := range r.Findings {
		if f.Severity == SeverityCritical {
			criticalCount++
		}
	}
	if criticalCount < 2 {
		t.Errorf("expected at least 2 critical findings for schema missing, got %d", criticalCount)
	}
}

func TestSyntheticStoresDownScenario(t *testing.T) {
	checks := SyntheticChecks("stores_down")
	d := New("synthetic", checks)
	r := d.Run(nil)
	codes := map[string]bool{}
	for _, f := range r.Findings {
		codes[f.Code] = true
	}
	for _, code := range []string{"PG_UNREACHABLE", "RUSTFS_UNREACHABLE", "NATS_UNREACHABLE"} {
		if !codes[code] {
			t.Errorf("missing finding: %s", code)
		}
	}
}

func TestSyntheticPartialResetScenario(t *testing.T) {
	checks := SyntheticChecks("partial_reset")
	d := New("synthetic", checks)
	r := d.Run(nil)
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

func TestSyntheticTierGapsScenario(t *testing.T) {
	checks := SyntheticChecks("tier_gaps")
	d := New("synthetic", checks)
	r := d.Run(nil)
	codes := map[string]bool{}
	for _, f := range r.Findings {
		codes[f.Code] = true
	}
	for _, code := range []string{"TIER1_ORPHAN", "TIER2_MISSING_TIER1", "ORPHAN_EXTRACTED"} {
		if !codes[code] {
			t.Errorf("missing finding: %s", code)
		}
	}
}

func TestSyntheticRecoveryMixedScenario(t *testing.T) {
	items := SyntheticRecoveryItems("mixed")
	p := NewPlanner(RecoverConfig{WorkspaceID: "synthetic"}, items, nil)
	plan := p.Plan(nil)

	counts := map[RecoveryClassification]int{}
	for _, item := range plan.Items {
		counts[item.Classification]++
	}

	// 2 pending + 1 processing + 2 retryable failed = 5 retryable
	if counts[ClassRetryable] != 5 {
		t.Errorf("retryable %d, want 5", counts[ClassRetryable])
	}
	// 2 terminal failed
	if counts[ClassTerminal] != 2 {
		t.Errorf("terminal %d, want 2", counts[ClassTerminal])
	}
	// 1 completed + 1 skipped = 2 converged
	if counts[ClassConverged] != 2 {
		t.Errorf("converged %d, want 2", counts[ClassConverged])
	}
	// 1 unclassified = not_recoverable
	if counts[ClassNotRecoverable] != 1 {
		t.Errorf("not_recoverable %d, want 1", counts[ClassNotRecoverable])
	}
}

func TestSyntheticRecoveryAllRetryableScenario(t *testing.T) {
	items := SyntheticRecoveryItems("all_retryable")
	p := NewPlanner(RecoverConfig{WorkspaceID: "synthetic"}, items, nil)
	plan := p.Plan(nil)

	for _, item := range plan.Items {
		if item.Classification != ClassRetryable {
			t.Errorf("item %s: classification %q, want retryable", item.DocID, item.Classification)
		}
	}
}

func TestSyntheticRecoveryAllTerminalScenario(t *testing.T) {
	items := SyntheticRecoveryItems("all_terminal")
	p := NewPlanner(RecoverConfig{WorkspaceID: "synthetic"}, items, nil)
	plan := p.Plan(nil)

	for _, item := range plan.Items {
		if item.Classification != ClassTerminal {
			t.Errorf("item %s: classification %q, want terminal", item.DocID, item.Classification)
		}
		if item.Action != ActionRefuse {
			t.Errorf("item %s: action %q, want refused", item.DocID, item.Action)
		}
	}
}

func TestSyntheticRecoveryAllConvergedScenario(t *testing.T) {
	items := SyntheticRecoveryItems("all_converged")
	p := NewPlanner(RecoverConfig{WorkspaceID: "synthetic"}, items, nil)
	plan := p.Plan(nil)

	for _, item := range plan.Items {
		if item.Classification != ClassConverged {
			t.Errorf("item %s: classification %q, want converged", item.DocID, item.Classification)
		}
	}
}

func TestSyntheticRecoveryEmptyScenario(t *testing.T) {
	items := SyntheticRecoveryItems("empty")
	p := NewPlanner(RecoverConfig{WorkspaceID: "synthetic"}, items, nil)
	plan := p.Plan(nil)

	if len(plan.Items) != 0 {
		t.Errorf("expected 0 items, got %d", len(plan.Items))
	}
	if plan.Summary == "" {
		t.Error("expected non-empty summary")
	}
}
