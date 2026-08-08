package cli

import (
	"strings"
	"testing"
)

func TestParseAcceptsTheDocumentedInvocations(t *testing.T) {
	cases := map[string][]string{
		"ingest-all":  {"--ingest-all", "--workspace-id", "test"},
		"delete-data": {"--delete-data", "--workspace-id", "test"},
		"scan":        {"--scan", "--workspace-id", "test"},
		"reconcile":   {"--reconcile", "--workspace-id", "test"},
		"doctor":      {"--doctor", "--workspace-id", "test"},
		"recover":     {"--recover", "--workspace-id", "test"},
	}
	for wantMode, args := range cases {
		o, err := Parse(args)
		if err != nil {
			t.Errorf("Parse(%v): %v", args, err)
			continue
		}
		if o.Mode() != wantMode {
			t.Errorf("Parse(%v).Mode() = %q, want %q", args, o.Mode(), wantMode)
		}
	}
}

// Selecting two modes at once is ambiguous in a way that could destroy data —
// --delete-data alongside --ingest-all must never be resolved by picking one.
func TestParseRejectsMultipleModes(t *testing.T) {
	_, err := Parse([]string{"--ingest-all", "--delete-data", "--workspace-id", "test"})
	if err == nil {
		t.Fatal("Parse accepted two modes at once")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error %q does not explain the conflict", err)
	}
}

func TestParseRequiresAMode(t *testing.T) {
	if _, err := Parse([]string{"--workspace-id", "test"}); err == nil {
		t.Fatal("Parse accepted an invocation with no mode")
	}
}

// A mode that defaulted its workspace could purge the wrong corpus. There is
// no shared database or bucket left to fall back to
// (workspace-isolation.md §13).
func TestParseRequiresWorkspace(t *testing.T) {
	for _, mode := range []string{
		"--ingest-all", "--delete-data", "--scan", "--reconcile",
		"--doctor", "--recover",
	} {
		if _, err := Parse([]string{mode}); err == nil {
			t.Errorf("Parse(%s) accepted a missing --workspace-id", mode)
		}
	}
}

func TestParseValidatesForgetHash(t *testing.T) {
	valid := strings.Repeat("a", 64)
	if _, err := Parse([]string{"--forget", valid, "--workspace-id", "test"}); err != nil {
		t.Errorf("Parse rejected a valid sha256: %v", err)
	}
	if _, err := Parse([]string{"--forget", "abc123", "--workspace-id", "test"}); err == nil {
		t.Error("Parse accepted a truncated sha256")
	}
}

// Every mode that enqueues must also drain it: nothing else is listening, so a
// mode that enqueued without running the pools would publish into a void.
func TestNeedsPipelineCoversEveryEnqueueingMode(t *testing.T) {
	enqueues := map[string]bool{
		"--ingest-all": true, "--scan": true, "--reconcile": true,
		"--delete-data": false, "--doctor": false, "--recover": false,
	}
	for mode, want := range enqueues {
		o, err := Parse([]string{mode, "--workspace-id", "test"})
		if err != nil {
			t.Fatalf("Parse(%s): %v", mode, err)
		}
		if got := o.NeedsPipeline(); got != want {
			t.Errorf("%s NeedsPipeline() = %v, want %v", mode, got, want)
		}
	}
}

func TestDoctorDoesNotNeedPipeline(t *testing.T) {
	o, err := Parse([]string{"--doctor", "--workspace-id", "test"})
	if err != nil {
		t.Fatal(err)
	}
	if o.NeedsPipeline() {
		t.Error("--doctor should not need pipeline")
	}
}

func TestRecoverDoesNotNeedPipeline(t *testing.T) {
	o, err := Parse([]string{"--recover", "--workspace-id", "test"})
	if err != nil {
		t.Fatal(err)
	}
	if o.NeedsPipeline() {
		t.Error("--recover should not need pipeline")
	}
}
