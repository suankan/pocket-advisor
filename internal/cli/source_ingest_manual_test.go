//go:build manual

package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ingestionv1 "github.com/suankan/pocket-advisor/api/proto/v1/gen"
	"github.com/suankan/pocket-advisor/internal/bus"
	"github.com/suankan/pocket-advisor/internal/client/embedding"
	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/retrieval"
	"github.com/suankan/pocket-advisor/internal/source"
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
	var cancelNext atomic.Pointer[context.CancelFunc]
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
		if cancel := cancelNext.Swap(nil); cancel != nil {
			(*cancel)()
		}
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
	o := &Options{IngestAll: true, WorkspaceID: id, WorkspaceConfig: registry, OCRLangs: "eng"}
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
	var scans int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM source_scans`).Scan(&scans); err != nil || scans != 2 {
		t.Fatalf("ingestion did not use exactly one scan per run: %d %v", scans, err)
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
	// Queue work before another rename. Workers must resolve the refreshed
	// occurrence, not park the job against the previous path.
	if _, err := db.Pool.Exec(ctx, `UPDATE ingestion_jobs SET state='WAITING_SOURCE' WHERE doc_id=$1`, before); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "renamed.txt"), filepath.Join(root, "final.txt")); err != nil {
		t.Fatal(err)
	}
	if err := runIngest(o, cfg, logs); err != nil {
		t.Fatalf("renamed queued source did not recover: %v", err)
	}
	// Ingestion never deletes. A removed original's waiting job is reported as
	// unresolved, naming the sweep, and its document survives the run.
	missingHash := domain.SHA256Hex([]byte("removed synthetic original"))
	missing, _, err := docs.CreateStub(ctx, &domain.Document{DocID: domain.NewDocID(), RawSHA256: missingHash, DocType: "pdf", SourceName: "missing.pdf"})
	if err != nil {
		t.Fatal(err)
	}
	jobs := postgres.NewJobs(db)
	if err := jobs.Publish(ctx, bus.SubjectPDFs, &ingestionv1.ProcessPdfCommand{
		Metadata: &ingestionv1.DocumentMetadata{DocId: missing, RawSha256: missingHash, Traceparent: "00-11111111111111111111111111111111-1111111111111111-01"}, SourceRef: source.Ref(missing)}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE ingestion_jobs SET state='WAITING_SOURCE' WHERE doc_id=$1`, missing); err != nil {
		t.Fatal(err)
	}
	if err := runIngest(o, cfg, logs); err == nil || !strings.Contains(err.Error(), "--reconcile-deletions") {
		t.Fatalf("waiting job on a removed original was not reported: %v", err)
	}
	if exists, err := docs.Exists(ctx, missing); err != nil || !exists {
		t.Fatalf("ingestion deleted a document: %v", err)
	}
	// A container that cannot be parsed fails extraction and produces no
	// members. Stop staging the direct attachment as well; the retained email
	// still carries it.
	bad := []byte("From: sender@example.test\r\nSubject: Synthetic bad charset\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=not-a-real-charset\r\n\r\nBody.\r\n")
	if err := os.WriteFile(filepath.Join(root, "bad.eml"), bad, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "final.txt")); err != nil {
		t.Fatal(err)
	}
	if err := runIngest(o, cfg, logs); err == nil {
		t.Fatal("failed extraction and a removed original were not reported")
	}
	badDoc, err := docs.DocIDBySHA(ctx, domain.SHA256Hex(bad))
	if err != nil || badDoc == "" {
		t.Fatalf("unparsable container not admitted: %v", err)
	}
	// The sweep is its own command. A preview deletes nothing; applying needs
	// --yes and a typed confirmation.
	answer := confirm
	defer func() { confirm = answer }()
	prompts := 0
	confirm = func(string) bool { prompts++; return true }
	sweep := &Options{ReconcileDeletions: true, WorkspaceID: id, WorkspaceConfig: registry}
	if err := runReconcileDeletions(sweep, cfg, logs); err != nil {
		t.Fatalf("sweep preview: %v", err)
	}
	if exists, err := docs.Exists(ctx, missing); err != nil || !exists || prompts != 0 {
		t.Fatalf("preview deleted or prompted: %d %v", prompts, err)
	}
	sweep.Yes = true
	if err := runReconcileDeletions(sweep, cfg, logs); err != nil {
		t.Fatalf("unparsable retained container blocked the sweep: %v", err)
	}
	if prompts != 1 {
		t.Fatalf("sweep applied without one confirmation: %d", prompts)
	}
	if exists, err := docs.Exists(ctx, missing); err != nil || exists {
		t.Fatalf("unreachable document survived the sweep: %v", err)
	}
	if exists, err := docs.Exists(ctx, before); err != nil || !exists {
		t.Fatalf("lost attachment carried by retained email: %v", err)
	}
	if exists, err := docs.Exists(ctx, badDoc); err != nil || !exists {
		t.Fatalf("swept a container whose original is present: %v", err)
	}
	// Once its original goes, the failed container is swept with its job and
	// ingestion resolves cleanly.
	if err := os.Remove(filepath.Join(root, "bad.eml")); err != nil {
		t.Fatal(err)
	}
	if err := runReconcileDeletions(sweep, cfg, logs); err != nil {
		t.Fatal(err)
	}
	if exists, err := docs.Exists(ctx, badDoc); err != nil || exists {
		t.Fatalf("failed container survived the sweep: %v", err)
	}
	if err := runIngest(o, cfg, logs); err != nil {
		t.Fatalf("swept workspace did not resolve: %v", err)
	}
	// Interruption is reported. A jobless row whose original is gone is left
	// alone by the next run rather than resumed into a missing source.
	missing, _, err = docs.CreateStub(ctx, &domain.Document{DocID: domain.NewDocID(), RawSHA256: missingHash, DocType: "pdf"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cancel.txt"), []byte("Synthetic work cancelled mid-run."), 0600); err != nil {
		t.Fatal(err)
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	cancelNext.Store(&cancel)
	err = runIngestContext(cancelCtx, o, cfg, logs)
	cancel()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interruption not reported: %v", err)
	}
	if err := runIngest(o, cfg, logs); err != nil {
		t.Fatalf("run after interruption did not resolve: %v", err)
	}
	if exists, err := docs.Exists(ctx, missing); err != nil || !exists {
		t.Fatalf("ingestion deleted a document: %v", err)
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
	for name, want := range map[string][]byte{"message.eml": mail, "cancel.txt": []byte("Synthetic work cancelled mid-run.")} {
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
