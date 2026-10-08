package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validInfra supplies every field requireInfra checks — none of them has a
// Go-side default any more (deviation 41), so a test about something else
// entirely (path anchoring, env overrides) would otherwise fail on an
// unrelated "missing infra.postgres.host" rather than testing what it
// means to test. Each test below adds its own workspaces: block on top.
const validInfra = `
infra:
  postgres:
    host: postgres.example
  reranking:
    endpoint: http://localhost:8000/v1/rerank
    model: test-rerank-model
  llm:
    endpoint: http://localhost:8000/v1/chat/completions
    model: test-llm-model
`

// The workspace registry path is the only configuration that names another
// file, so it is the only configuration whose meaning depends on where it is
// read from. Every case below is about that anchor, because getting it wrong
// is silent from the repository root and fatal anywhere else — which is how
// an MCP client runs this binary.

func writeConfig(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWorkspacePathsResolveAgainstTheConfigFile(t *testing.T) {
	// Deliberately not t.Chdir: the point is that the working directory is
	// irrelevant. The test binary runs in the package directory, so a config
	// read from tmp proves the anchor is the file rather than the cwd.
	tmp := t.TempDir()
	path := writeConfig(t, tmp, validInfra+`
workspaces:
  config: workspaces/workspaces.yaml
`)

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	wantConfig := filepath.Join(tmp, "workspaces/workspaces.yaml")
	if c.WorkspacesConfigPath != wantConfig {
		t.Errorf("registry path = %q, want %q", c.WorkspacesConfigPath, wantConfig)
	}
}

func TestAbsoluteWorkspacePathsAreLeftAlone(t *testing.T) {
	// An absolute path is already unambiguous, and rewriting it would break
	// pointing at a registry that lives outside the repository.
	tmp := t.TempDir()
	elsewhere := t.TempDir()
	abs := filepath.Join(elsewhere, "registry.yaml")
	path := writeConfig(t, tmp, validInfra+"\nworkspaces:\n  config: "+abs+"\n")

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.WorkspacesConfigPath != abs {
		t.Errorf("registry path = %q, want it unchanged at %q", c.WorkspacesConfigPath, abs)
	}
}

func TestEnvironmentOverrideIsTakenLiterally(t *testing.T) {
	// The environment is set by whoever launches the process, in their own
	// working directory, so it wins over the file and is not re-anchored to it.
	tmp := t.TempDir()
	path := writeConfig(t, tmp, validInfra+"\nworkspaces:\n  config: workspaces/workspaces.yaml\n")
	t.Setenv("WORKSPACES_CONFIG", "some/other/registry.yaml")

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.WorkspacesConfigPath != "some/other/registry.yaml" {
		t.Errorf("registry path = %q, want the environment's value verbatim", c.WorkspacesConfigPath)
	}
}

// LogDir must resolve exactly like WorkspacesConfigPath: anchored to
// config.yaml's own directory, not the process's working directory. This is
// a regression test for a real bug: the resolution used to run before
// setStr(&c.LogDir, in.Observability.LogDir) applied the file's own
// (typically still-relative) value, so setStr silently overwrote the
// resolved absolute path with the unresolved one straight back whenever
// config.yaml set observability.log_dir at all — which the committed
// config.yaml does. It was invisible from the repository root, where the
// unresolved relative path still happened to work by coincidence, and only
// became a hard failure ("mkdir logs: read-only file system") once an MCP
// client launched mcp stdio from a working directory the process could not
// write to.
func TestLogDirResolvesAgainstTheConfigFile(t *testing.T) {
	tmp := t.TempDir()
	path := writeConfig(t, tmp, validInfra+`
  observability:
    log_dir: logs
workspaces:
  config: workspaces/workspaces.yaml
`)

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(tmp, "logs")
	if c.LogDir != want {
		t.Errorf("LogDir = %q, want %q", c.LogDir, want)
	}
}

func TestLogDirResolvesAgainstTheConfigFileEvenWithoutAnExplicitValue(t *testing.T) {
	// No observability.log_dir at all: setStr is then a no-op, and it is the
	// Go-side default "logs" (from defaults()) that must still be resolved,
	// not left relative.
	tmp := t.TempDir()
	path := writeConfig(t, tmp, validInfra+`
workspaces:
  config: workspaces/workspaces.yaml
`)

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(tmp, "logs")
	if c.LogDir != want {
		t.Errorf("LogDir = %q, want %q", c.LogDir, want)
	}
}

func TestAbsoluteLogDirIsLeftAlone(t *testing.T) {
	tmp := t.TempDir()
	elsewhere := t.TempDir()
	path := writeConfig(t, tmp, validInfra+`
  observability:
    log_dir: `+elsewhere+`
workspaces:
  config: workspaces/workspaces.yaml
`)

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.LogDir != elsewhere {
		t.Errorf("LogDir = %q, want it unchanged at %q", c.LogDir, elsewhere)
	}
}

func TestLogDirEnvironmentOverrideIsTakenLiterally(t *testing.T) {
	tmp := t.TempDir()
	path := writeConfig(t, tmp, validInfra+`
  observability:
    log_dir: logs
workspaces:
  config: workspaces/workspaces.yaml
`)
	t.Setenv("LOG_DIR", "some/other/logs")

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.LogDir != "some/other/logs" {
		t.Errorf("LogDir = %q, want the environment's value verbatim", c.LogDir)
	}
}

func TestMissingConfigFileIsAConfigurationError(t *testing.T) {
	// A missing file used to fall back to a built-in default describing a
	// stock local cluster. There is no such default left (deviation 41) —
	// config.yaml is the only source of truth for these fields now, so a
	// missing file leaves them all empty and Load must refuse to proceed.
	_, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err == nil {
		t.Fatal("want an error for a missing config file, got nil")
	}
}

func TestConfigExpandsRequiredEnvironmentPlaceholders(t *testing.T) {
	tmp := t.TempDir()
	path := writeConfig(t, tmp, strings.Replace(validInfra, "postgres.example", "${TEST_POSTGRES_HOST}", 1)+`
workspaces:
  config: workspaces/workspaces.yaml
`)
	t.Setenv("TEST_POSTGRES_HOST", "postgres.from-env")

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Postgres.Host != "postgres.from-env" {
		t.Errorf("Postgres.Host = %q, want expanded value", c.Postgres.Host)
	}
}

func TestConfigRejectsUnsetEnvironmentPlaceholder(t *testing.T) {
	tmp := t.TempDir()
	path := writeConfig(t, tmp, strings.Replace(validInfra, "postgres.example", "${UNSET_POSTGRES_HOST}", 1))

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "UNSET_POSTGRES_HOST") {
		t.Fatalf("Load error = %v, want unset placeholder error", err)
	}
}

func TestBareConfigNameKeepsPathsUnchanged(t *testing.T) {
	// filepath.Dir("config.yaml") is ".", and joining against it must be a
	// no-op — this is the path every command run from the repository root
	// takes, and it has to behave exactly as it did before anchoring existed.
	if got := resolveAgainst(".", "workspaces/workspaces.yaml"); got != "workspaces/workspaces.yaml" {
		t.Errorf("resolveAgainst(\".\", …) = %q, want it unchanged", got)
	}
}
