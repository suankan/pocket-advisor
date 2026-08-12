package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/suankan/pocket-advisor/internal/config"
)

func TestParseExtractsConfigForMCPStart(t *testing.T) {
	o, err := Parse([]string{"mcp", "start", "--workspace-id", "test", "--config", "/tmp/custom-config.yaml", "--addr", "127.0.0.1:9090"})
	if err != nil {
		t.Fatal(err)
	}
	if o.SubCommand != "mcp start" {
		t.Fatalf("SubCommand = %q", o.SubCommand)
	}
	if o.ConfigPath != "/tmp/custom-config.yaml" {
		t.Fatalf("ConfigPath = %q", o.ConfigPath)
	}
	want := []string{"--workspace-id", "test", "--addr", "127.0.0.1:9090"}
	if len(o.SubArgs) != len(want) {
		t.Fatalf("SubArgs = %v", o.SubArgs)
	}
	for i, v := range want {
		if o.SubArgs[i] != v {
			t.Fatalf("SubArgs = %v, want %v", o.SubArgs, want)
		}
	}
}

func TestScanFlagValue(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"double dash space", []string{"--addr", "127.0.0.1:9090", "--workspace-id", "test"}, "test"},
		{"single dash space", []string{"-workspace-id", "test"}, "test"},
		{"double dash equals", []string{"--workspace-id=test"}, "test"},
		{"single dash equals", []string{"-workspace-id=test"}, "test"},
		{"absent", []string{"--addr", "127.0.0.1:9090"}, ""},
		{"trailing without value", []string{"--workspace-id"}, ""},
		{"empty", nil, ""},
	}
	for _, c := range cases {
		if got := scanFlagValue(c.args, "workspace-id"); got != c.want {
			t.Errorf("%s: scanFlagValue(%v) = %q, want %q", c.name, c.args, got, c.want)
		}
	}
}

func TestDaemonChildArgsPrefixesConfigBeforeOriginalArgs(t *testing.T) {
	got := daemonChildArgs("/abs/config.yaml", []string{"--workspace-id", "test", "--addr", "127.0.0.1:9090"})
	want := []string{"mcp", "start", "--config", "/abs/config.yaml", "--workspace-id", "test", "--addr", "127.0.0.1:9090"}
	if len(got) != len(want) {
		t.Fatalf("daemonChildArgs = %v", got)
	}
	for i, v := range want {
		if got[i] != v {
			t.Fatalf("daemonChildArgs = %v, want %v", got, want)
		}
	}
}

func TestResolveConfigPath(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	got, err := resolveConfigPath("")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(wd, config.DefaultPath)
	if got != want {
		t.Errorf("resolveConfigPath(\"\") = %q, want %q", got, want)
	}

	got, err = resolveConfigPath("relative/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want = filepath.Join(wd, "relative/config.yaml")
	if got != want {
		t.Errorf("resolveConfigPath(relative) = %q, want %q", got, want)
	}

	got, err = resolveConfigPath("/already/absolute/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/already/absolute/config.yaml" {
		t.Errorf("resolveConfigPath(absolute) = %q, want unchanged", got)
	}
}

// daemonizeMCPStart must refuse to fork a second daemon for a workspace whose
// PID file already names a live process, and must do so from the pre-check
// alone — before ever touching the filesystem for a log file or attempting
// exec.Command. This test proves the refusal without a real daemon: the
// recorded PID is this test process's own, which readLivePID (Signal 0) sees
// as alive without actually needing the process to be an mcp server.
func TestDaemonizeMCPStartRefusesADuplicateWorkspace(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{LogDir: dir}
	workspaceID := "already-running"
	pidPath := mcpPIDPath(cfg, workspaceID)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}

	err := daemonizeMCPStart(cfg, "", []string{"--workspace-id", workspaceID})
	if err == nil {
		t.Fatal("daemonizeMCPStart did not refuse a duplicate workspace")
	}
	if got := err.Error(); got == "" {
		t.Fatal("empty error")
	}
}

func TestDaemonizeMCPStartRequiresWorkspaceID(t *testing.T) {
	cfg := &config.Config{LogDir: t.TempDir()}
	if err := daemonizeMCPStart(cfg, "", nil); err == nil {
		t.Fatal("daemonizeMCPStart accepted a missing --workspace-id")
	}
}
