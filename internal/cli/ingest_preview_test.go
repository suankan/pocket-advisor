package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/telemetry"
)

func TestIngestDryRunNeedsNoDatabaseBrokerOrModelAndNeverWrites(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "document.txt"), []byte("synthetic content"), 0600); err != nil {
		t.Fatal(err)
	}
	registry := filepath.Join(t.TempDir(), "registry.yaml")
	if err := os.WriteFile(registry, []byte("schema_version: 2\nworkspaces:\n  - id: synthetic\n    paths:\n      - "+root+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	logs, err := telemetry.OpenLogs(t.TempDir(), "error")
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	cfg := &config.Config{}
	o := &Options{IngestAll: true, DryRun: true, WorkspaceID: "synthetic", WorkspaceConfig: registry}
	if err := runIngest(o, cfg, logs); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "document.txt"))
	if err != nil || string(data) != "synthetic content" {
		t.Fatalf("source changed: %q, %v", data, err)
	}
}

func TestPruneDryRunDoesNotLoadWorkspaceOrTouchStores(t *testing.T) {
	if err := prune(context.Background(), &Options{DryRun: true}, nil, postgres.SourceScan{}); err != nil {
		t.Fatal(err)
	}
}
