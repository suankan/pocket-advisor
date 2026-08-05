package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The workspace paths are the only configuration that names another file, so
// they are the only configuration whose meaning depends on where it is read
// from. Every case below is about that anchor, because getting it wrong is
// silent from the repository root and fatal anywhere else — which is how an MCP
// client runs this binary.

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
	path := writeConfig(t, tmp, `
workspaces:
  config: workspaces/workspace-config.yaml
  values: workspaces/pocket-advisor-infra.yaml
`)

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	wantConfig := filepath.Join(tmp, "workspaces/workspace-config.yaml")
	wantValues := filepath.Join(tmp, "workspaces/pocket-advisor-infra.yaml")
	if c.WorkspacesConfigPath != wantConfig {
		t.Errorf("registry path = %q, want %q", c.WorkspacesConfigPath, wantConfig)
	}
	if c.WorkspacesValuesPath != wantValues {
		t.Errorf("values path = %q, want %q", c.WorkspacesValuesPath, wantValues)
	}
}

func TestWorkspaceDefaultsResolveAgainstTheConfigFile(t *testing.T) {
	// A config that says nothing about workspaces still has to find them: the
	// built-in defaults describe the same repository layout, so they anchor the
	// same way. This is the case a minimal config.yaml actually hits.
	tmp := t.TempDir()
	path := writeConfig(t, tmp, "infra:\n  nats:\n    url: nats://example:4222\n")

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(tmp, defaultWorkspacesValuesPath)
	if c.WorkspacesValuesPath != want {
		t.Errorf("values path = %q, want %q", c.WorkspacesValuesPath, want)
	}
}

func TestAbsoluteWorkspacePathsAreLeftAlone(t *testing.T) {
	// An absolute path is already unambiguous, and rewriting it would break
	// pointing at a registry that lives outside the repository.
	tmp := t.TempDir()
	elsewhere := t.TempDir()
	abs := filepath.Join(elsewhere, "registry.yaml")
	path := writeConfig(t, tmp, "workspaces:\n  config: "+abs+"\n")

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
	path := writeConfig(t, tmp, "workspaces:\n  values: workspaces/pocket-advisor-infra.yaml\n")
	t.Setenv("WORKSPACES_VALUES", "some/other/values.yaml")

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.WorkspacesValuesPath != "some/other/values.yaml" {
		t.Errorf("values path = %q, want the environment's value verbatim", c.WorkspacesValuesPath)
	}
}

func TestMissingConfigFileLeavesDefaultsRelative(t *testing.T) {
	// A missing file is not an error — the defaults describe a stock local
	// cluster. With no file there is nothing to anchor to, so the paths stay as
	// they were rather than being anchored to a directory that does not exist.
	c, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.WorkspacesValuesPath != defaultWorkspacesValuesPath {
		t.Errorf("values path = %q, want the unanchored default %q",
			c.WorkspacesValuesPath, defaultWorkspacesValuesPath)
	}
}

func TestBareConfigNameKeepsPathsUnchanged(t *testing.T) {
	// filepath.Dir("config.yaml") is ".", and joining against it must be a
	// no-op — this is the path every command run from the repository root
	// takes, and it has to behave exactly as it did before anchoring existed.
	if got := resolveAgainst(".", "workspaces/pocket-advisor-infra.yaml"); got != "workspaces/pocket-advisor-infra.yaml" {
		t.Errorf("resolveAgainst(\".\", …) = %q, want it unchanged", got)
	}
}
