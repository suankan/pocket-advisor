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
// unrelated "missing infra.rustfs.endpoint" rather than testing what it
// means to test. Each test below adds its own workspaces: block on top.
const validInfra = `
infra:
  rustfs:
    endpoint: rustfs.example:9000
  nats:
    url: nats://nats.example:4222
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
  config: workspaces/workspace-config.yaml
`)

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	wantConfig := filepath.Join(tmp, "workspaces/workspace-config.yaml")
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
	path := writeConfig(t, tmp, validInfra+"\nworkspaces:\n  config: workspaces/workspace-config.yaml\n")
	t.Setenv("WORKSPACES_CONFIG", "some/other/registry.yaml")

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.WorkspacesConfigPath != "some/other/registry.yaml" {
		t.Errorf("registry path = %q, want the environment's value verbatim", c.WorkspacesConfigPath)
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
	path := writeConfig(t, tmp, strings.Replace(validInfra, "rustfs.example:9000", "${TEST_RUSTFS_ENDPOINT}", 1)+`
workspaces:
  config: workspaces/workspace-config.yaml
`)
	t.Setenv("TEST_RUSTFS_ENDPOINT", "rustfs.from-env:9000")

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.RustFS.Endpoint != "rustfs.from-env:9000" {
		t.Errorf("RustFS.Endpoint = %q, want expanded value", c.RustFS.Endpoint)
	}
}

func TestConfigRejectsUnsetEnvironmentPlaceholder(t *testing.T) {
	tmp := t.TempDir()
	path := writeConfig(t, tmp, strings.Replace(validInfra, "rustfs.example:9000", "${UNSET_RUSTFS_ENDPOINT}", 1))

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "UNSET_RUSTFS_ENDPOINT") {
		t.Fatalf("Load error = %v, want unset placeholder error", err)
	}
}

func TestBareConfigNameKeepsPathsUnchanged(t *testing.T) {
	// filepath.Dir("config.yaml") is ".", and joining against it must be a
	// no-op — this is the path every command run from the repository root
	// takes, and it has to behave exactly as it did before anchoring existed.
	if got := resolveAgainst(".", "workspaces/workspace-config.yaml"); got != "workspaces/workspace-config.yaml" {
		t.Errorf("resolveAgainst(\".\", …) = %q, want it unchanged", got)
	}
}

func TestTopicGraphRelationContractLoads(t *testing.T) {
	tmp := t.TempDir()
	path := writeConfig(t, tmp, validInfra+`
  topic_graph:
    relation_version: relations-v1
    relation_prompt_version: relation-prompt-v1
    relation_max_input_bytes: 100
    relation_max_output_bytes: 200
    relation_max_output_tokens: 30
    relation_max_candidates: 7
    relation_min_confidence: 0.8
workspaces:
  config: registry.yaml
`)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.TopicGraph.RelationVersion != "relations-v1" || c.TopicGraph.RelationPromptVersion != "relation-prompt-v1" || c.TopicGraph.RelationMaxCandidates != 7 || c.TopicGraph.RelationMinConfidence != .8 {
		t.Fatalf("relation topic graph config = %#v", c.TopicGraph)
	}
}
