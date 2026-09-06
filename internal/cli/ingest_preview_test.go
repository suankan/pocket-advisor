package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/telemetry"
)

func TestIngestDryRunNeedsNoDatabaseBrokerOrModelAndNeverWrites(t *testing.T) {
	var reads, writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writes.Add(1)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		reads.Add(1)
		if _, ok := r.URL.Query()["location"]; ok {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">us-east-1</LocationConstraint>`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
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
	cfg := &config.Config{RustFS: config.RustFS{Endpoint: strings.TrimPrefix(server.URL, "http://")}}
	o := &Options{IngestAll: true, DryRun: true, WorkspaceID: "synthetic", WorkspaceConfig: registry}
	if err := runIngest(o, cfg, logs); err != nil {
		t.Fatal(err)
	}
	if reads.Load() == 0 || writes.Load() != 0 {
		t.Fatalf("reads=%d writes=%d", reads.Load(), writes.Load())
	}
}

func TestPruneDryRunDoesNotLoadWorkspaceOrTouchStores(t *testing.T) {
	if err := prune(context.Background(), &Options{DryRun: true}, nil); err != nil {
		t.Fatal(err)
	}
}
