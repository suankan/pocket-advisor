//go:build manual

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/suankan/pocket-advisor/internal/storage/postgres"
)

// A real ingestion process is killed with SIGKILL while handlers are running,
// then restarted. Work it owned stays RUNNING until its lease expires; after
// expiry the next run completes it without duplicating any stage effect. A
// poison job that crashes the process on every attempt spends exactly its three
// attempts and is then failed without a fourth handler.
//
//	Run: EMAIL_DSN=postgres://postgres@127.0.0.1:<port>/<disposable-db> \
//		mise exec -- go test -tags ocr,manual ./internal/cli/ -run Killed -v
//
// EMAIL_DSN needs superuser rights on a disposable database. The test builds
// the binary itself, uses a synthetic embedding endpoint, and touches nothing
// else. Lease expiry is made deterministic by rewriting lease_until in the
// database rather than sleeping five minutes.
func TestKilledIngestionResumesWithoutDuplicatesAndExhaustsItsBudget(t *testing.T) {
	dsn := os.Getenv("EMAIL_DSN")
	if dsn == "" {
		t.Skip("requires a disposable EMAIL_DSN with superuser rights")
	}
	ctx := context.Background()
	const id = "kill_e2e"

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := postgres.Connect(ctx, dsn, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	for _, sql := range []string{
		`DROP DATABASE IF EXISTS ` + id + ` WITH (FORCE)`, `DROP ROLE IF EXISTS ` + id,
		`CREATE ROLE ` + id + ` LOGIN`, `CREATE DATABASE ` + id + ` OWNER ` + id,
	} {
		if _, err := admin.Pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		_, _ = admin.Pool.Exec(ctx, `DROP DATABASE `+id+` WITH (FORCE)`)
		_, _ = admin.Pool.Exec(ctx, `DROP ROLE `+id)
	}()
	setupURL := *u
	setupURL.Path = "/" + id
	setup, err := postgres.Connect(ctx, setupURL.String(), 4)
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{`CREATE EXTENSION vector`, `CREATE EXTENSION pg_textsearch`} {
		if _, err := setup.Pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	setup.Close()

	// The synthetic endpoint answers ordinary text at once and holds any request
	// containing the poison marker open until the caller is killed.
	var poisonSeen atomic.Int64
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":[{"id":"synthetic","dimensions":8}]}`))
			return
		}
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return
		}
		for _, in := range req.Input {
			if strings.Contains(in, "POISONMARKER") {
				poisonSeen.Add(1)
				<-r.Context().Done() // the process is killed while this is open
				return
			}
		}
		data := make([]map[string]any, 0, len(req.Input))
		for i := range req.Input {
			data = append(data, map[string]any{"index": i, "embedding": []float32{1, .2, .3, .4, .5, .6, .7, .8}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "model": "synthetic"})
	}))
	defer model.Close()

	root := t.TempDir()
	const normal = 14
	for i := 0; i < normal; i++ {
		body := fmt.Sprintf("Synthetic lighthouse note %d. %s", i, strings.Repeat(fmt.Sprintf("harbour tide %d beacon. ", i), 40))
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("note-%02d.txt", i)), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "poison.txt"), []byte("POISONMARKER a document that crashes its worker every time"), 0o600); err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	registry := filepath.Join(work, "registry.yaml")
	if err := os.WriteFile(registry, []byte(fmt.Sprintf("schema_version: 2\nworkspaces:\n  - id: %s\n    paths: [%q]\n", id, root)), 0o600); err != nil {
		t.Fatal(err)
	}
	port := u.Port()
	config := filepath.Join(work, "config.yaml")
	yaml := fmt.Sprintf(`infra:
  postgres:
    host: %s
    port: %s
    sslmode: disable
    max_conns: 12
  embedding:
    endpoint: %s/v1/embeddings
    model: synthetic
    timeout: 60s
    concurrency: 4
  reranking:
    endpoint: %s/v1/rerank
    model: synthetic
  llm:
    endpoint: %s/v1/chat
    model: synthetic
  observability:
    metrics_port: 0
    log_level: error
    log_dir: %s
workspaces:
  config: %s
`, u.Hostname(), port, model.URL, model.URL, model.URL, filepath.Join(work, "logs"), registry)
	if err := os.WriteFile(config, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(work, "pocket-advisor")
	build := exec.Command("go", "build", "-tags", "ocr", "-o", bin, "../../cmd/pocket-advisor")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	dbURL := *u
	dbURL.User = url.User(id)
	dbURL.Path = "/" + id
	db, err := postgres.Connect(ctx, dbURL.String(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	start := func() (*exec.Cmd, *bytes.Buffer) {
		var out bytes.Buffer
		cmd := exec.Command(bin, "--ingest-all", "--workspace-id", id, "--config", config, "--workspace-config", registry)
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd, &out
	}
	expireLeases := func() int64 {
		tag, err := db.Pool.Exec(ctx, `UPDATE ingestion_jobs SET lease_until = clock_timestamp() - interval '1 second' WHERE state = 'RUNNING'`)
		if err != nil {
			t.Fatal(err)
		}
		return tag.RowsAffected()
	}
	poisonRow := func() (state string, attempts int, reason string) {
		err := db.Pool.QueryRow(ctx, `SELECT j.state, j.attempts, j.reason FROM ingestion_jobs j JOIN documents d USING (doc_id)
 WHERE d.source_filename = 'poison.txt' AND j.subject = $1`, postgres.SubjectEmbed).Scan(&state, &attempts, &reason)
		if err != nil {
			t.Fatalf("poison job: %v", err)
		}
		return
	}

	// Three runs, each killed while the poison document's handler is running.
	for cycle := 1; cycle <= postgres.MaxAttempts; cycle++ {
		cmd, out := start()
		deadline := time.Now().Add(90 * time.Second)
		for poisonSeen.Load() < int64(cycle) {
			if time.Now().After(deadline) {
				_ = cmd.Process.Kill()
				t.Fatalf("cycle %d: the poison handler never started\n%s", cycle, out.String())
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err := cmd.Process.Kill(); err != nil { // SIGKILL: no cleanup, no verdicts
			t.Fatal(err)
		}
		_ = cmd.Wait()
		state, attempts, _ := poisonRow()
		if state != "RUNNING" || attempts != cycle {
			t.Fatalf("cycle %d: poison job = %s with %d attempts, want RUNNING with %d", cycle, state, attempts, cycle)
		}
		// Before expiry the dead process still appears to own its work.
		if got := expireLeases(); got < 1 {
			t.Fatalf("cycle %d: no RUNNING job to expire", cycle)
		}
	}

	// The fourth run reclaims the poison job with its budget spent: it must fail
	// it without running a handler, and finish everything else.
	cmd, out := start()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("a run that failed a job exited cleanly\n%s", out.String())
		}
		if !strings.Contains(out.String(), "1 jobs failed in this run") {
			t.Fatalf("the failure was not reported as unresolved work:\n%s", out.String())
		}
	case <-time.After(120 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("the final run did not finish\n%s", out.String())
	}
	if got := poisonSeen.Load(); got != int64(postgres.MaxAttempts) {
		t.Fatalf("the poison handler ran %d times, want exactly %d: a fourth handler ran", got, postgres.MaxAttempts)
	}
	state, attempts, reason := poisonRow()
	if state != "FAILED" || attempts != postgres.MaxAttempts || reason != "UNCLASSIFIED" {
		t.Fatalf("poison job = %s, %d attempts, reason %q; want an actionable FAILED row", state, attempts, reason)
	}

	// Every ordinary document completed once, with no duplicated stage effect.
	var docs, completed int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE d.source_filename LIKE 'note-%'),
 count(*) FILTER (WHERE d.source_filename LIKE 'note-%' AND d.processing_status = 'COMPLETED') FROM documents d`).Scan(&docs, &completed); err != nil {
		t.Fatal(err)
	}
	if docs != normal || completed != normal {
		t.Fatalf("%d of %d ordinary documents completed", completed, normal)
	}
	var duplicated int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM (SELECT doc_id, chunk_index FROM document_chunks GROUP BY 1, 2 HAVING count(*) > 1) d`).Scan(&duplicated); err != nil || duplicated != 0 {
		t.Fatalf("%d duplicated chunk placements after crash recovery (%v)", duplicated, err)
	}
	var perDoc int
	if err := db.Pool.QueryRow(ctx, `SELECT count(DISTINCT n) FROM (SELECT count(*) AS n FROM document_chunks c JOIN documents d USING (doc_id)
 WHERE d.source_filename LIKE 'note-%' GROUP BY doc_id) x`).Scan(&perDoc); err != nil || perDoc != 1 {
		t.Fatalf("ordinary documents have differing chunk counts (%d distinct, %v): a document was partly or twice written", perDoc, err)
	}
	if n := 0; db.Pool.QueryRow(ctx, `SELECT count(*) FROM ingestion_jobs WHERE state IN ('READY','RUNNING')`).Scan(&n) != nil || n != 0 {
		t.Fatalf("%d jobs still unfinished", n)
	}
}
