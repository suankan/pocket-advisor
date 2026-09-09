//go:build manual

package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/suankan/pocket-advisor/internal/bus"
	"github.com/suankan/pocket-advisor/internal/client/embedding"
	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/retrieval"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/telemetry"
)

func TestSourceIngestEndToEnd(t *testing.T) {
	dsn, natsURL := os.Getenv("EMAIL_DSN"), os.Getenv("SOURCE_TEST_NATS")
	if dsn == "" || natsURL == "" {
		t.Skip("requires disposable EMAIL_DSN and SOURCE_TEST_NATS")
	}
	ctx := context.Background()
	admin, err := postgres.Connect(ctx, dsn, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	id := "source_e2e"
	for _, sql := range []string{`DROP DATABASE IF EXISTS source_e2e WITH (FORCE)`, `DROP ROLE IF EXISTS source_e2e`, `CREATE ROLE source_e2e LOGIN`, `CREATE DATABASE source_e2e OWNER source_e2e`} {
		if _, err := admin.Pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		_, _ = admin.Pool.Exec(ctx, `DROP DATABASE source_e2e WITH (FORCE)`)
		_, _ = admin.Pool.Exec(ctx, `DROP ROLE source_e2e`)
	}()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + id
	setup, err := postgres.Connect(ctx, u.String(), 4)
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{`CREATE EXTENSION vector`, `CREATE EXTENSION pg_textsearch`} {
		if _, err := setup.Pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	setup.Close()
	var embeds atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			_, _ = w.Write([]byte(`{"data":[{"id":"synthetic","dimensions":8}]}`))
			return
		}
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		embeds.Add(int32(len(req.Input)))
		data := []map[string]any{}
		for i := range req.Input {
			data = append(data, map[string]any{"index": i, "embedding": []float32{1, .2, .3, .4, .5, .6, .7, .8}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "model": "synthetic"})
	}))
	defer model.Close()
	root := t.TempDir()
	text := []byte("Synthetic lighthouse schedule. Arrival is Tuesday. Привет мир.")
	if err := os.WriteFile(filepath.Join(root, "note.txt"), text, 0600); err != nil {
		t.Fatal(err)
	}
	mail := []byte("From: sender@example.test\r\nTo: owner@example.test\r\nMessage-ID: <source@example.test>\r\nSubject: Lighthouse\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nPlease review the lighthouse schedule.\r\n--b\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=note.txt\r\nContent-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString(text) + "\r\n--b--\r\n")
	if err := os.WriteFile(filepath.Join(root, "message.eml"), mail, 0600); err != nil {
		t.Fatal(err)
	}
	registry := filepath.Join(t.TempDir(), "registry.yaml")
	if err := os.WriteFile(registry, []byte(fmt.Sprintf("schema_version: 2\nworkspaces:\n  - id: %s\n    paths: [%q]\n", id, root)), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load("../../config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Postgres.Host = u.Hostname()
	port, _ := strconv.Atoi(u.Port())
	cfg.Postgres.Port = port
	cfg.Postgres.MaxConns = 12
	cfg.NATS.URL = natsURL
	cfg.WorkspacesConfigPath = registry
	cfg.Embedding = config.Embedding{Endpoint: model.URL + "/embeddings", Model: "synthetic", Timeout: 5 * time.Second}
	cfg.MetricsPort = 0
	b, err := bus.Connect(ctx, natsURL, id)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.EnsureStreams(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.PurgeQueues(ctx); err != nil {
		t.Fatal(err)
	}
	logs, err := telemetry.OpenLogs(t.TempDir(), "error")
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	o := &Options{IngestAll: true, WorkspaceID: id, WorkspaceConfig: registry, HighWater: 100, LowWater: 50, NoDashboard: true, OCRLangs: "eng", PruneMaxShare: 1}
	if err := runIngest(o, cfg, logs); err != nil {
		t.Fatal(err)
	}
	first := embeds.Load()
	if first < 2 {
		t.Fatalf("no indexed documents: %d", first)
	}
	dbdsn, _ := cfg.WorkspacePostgresDSN(id)
	db, err := postgres.Connect(ctx, dbdsn, 6)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	docs := postgres.NewDocumentRepo(db)
	before, err := docs.DocIDBySHA(ctx, domain.SHA256Hex(text))
	if err != nil || before == "" {
		t.Fatalf("document missing %v", err)
	}
	if err := os.Rename(filepath.Join(root, "note.txt"), filepath.Join(root, "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	if err := runIngest(o, cfg, logs); err != nil {
		t.Fatal(err)
	}
	if embeds.Load() != first {
		t.Fatal("unchanged bytes re-embedded")
	}
	after, _ := docs.DocIDBySHA(ctx, domain.SHA256Hex(text))
	if before != after {
		t.Fatal("rename changed UUID")
	}
	locations, err := postgres.NewSourceRepo(db).Locations(ctx, []string{before})
	if err != nil || len(locations[before]) < 2 {
		t.Fatalf("lost duplicate/container locations %v %v", locations, err)
	}
	off := false
	svc := retrieval.New(db, embedding.New(cfg.Embedding), nil, nil, cfg.Query, id, logs.Logger(telemetry.RoleApp))
	result, err := svc.Query(ctx, retrieval.Request{Question: "lighthouse", TopK: 2, Rerank: &off, Decompose: &off})
	if err != nil || len(result.Packets) == 0 {
		t.Fatalf("hybrid retrieval: %v", err)
	}
	for _, p := range result.Packets {
		if len(p.SourceLocations) == 0 || p.SourceRef == "" {
			t.Fatal("missing original location")
		}
		if p.Match.SliceSHA256 == "" || p.ExtractionHash == "" {
			t.Fatal("missing exact provenance")
		}
	}
	// Offline roots do not invalidate cached evidence or prevent derived reset.
	offline := filepath.Join(t.TempDir(), "offline")
	if err := os.Rename(root, offline); err != nil {
		t.Fatal(err)
	}
	if err := runQuery(&Options{WorkspaceID: id, WorkspaceConfig: registry, Query: "lighthouse", TopK: 2, NoRerank: true, NoDecompose: true}, cfg, logs); err != nil {
		t.Fatalf("offline source blocked cached retrieval: %v", err)
	}
	// Forget derived rows without touching originals, including shared passages.
	if err := runReset(&Options{DeleteData: true, Yes: true, WorkspaceID: id, WorkspaceConfig: registry}, cfg, logs); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]byte{"renamed.txt": text, "message.eml": mail} {
		got, err := os.ReadFile(filepath.Join(offline, name))
		if err != nil || string(got) != string(want) {
			t.Fatalf("source modified: %s", name)
		}
	}
	var passages int
	if err := db.Pool.QueryRow(ctx, "SELECT count(*) FROM chunks").Scan(&passages); err != nil || passages != 0 {
		t.Fatalf("reset retained passage text: %d %v", passages, err)
	}
}
