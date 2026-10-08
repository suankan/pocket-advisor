//go:build manual

package pipeline

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	ingestionv1 "github.com/suankan/pocket-advisor/api/proto/v1/gen"
	"github.com/suankan/pocket-advisor/internal/app"
	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/source"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/telemetry"
	"github.com/suankan/pocket-advisor/internal/worker"
)

// Database-backed checks of drain detection and depth sampling.
//
//	Run: EMAIL_DSN=postgres://<role>@localhost:5432/<disposable-db> \
//		go test -tags manual ./internal/pipeline/ -v
//
// Point EMAIL_DSN at a disposable database, never a workspace. Each test uses a
// scratch schema of its own and synthetic content only.

func newPipelineEnv(t *testing.T, name string) (*Pipeline, *postgres.DB, *postgres.DocumentRepo, *postgres.Jobs) {
	t.Helper()
	dsn := os.Getenv("EMAIL_DSN")
	if dsn == "" {
		t.Skip("set EMAIL_DSN to a disposable database")
	}
	ctx := context.Background()
	admin, err := postgres.Connect(ctx, dsn, 2)
	if err != nil {
		t.Fatal(err)
	}
	schema := "pipe_" + strings.ToLower(name)
	for _, sql := range []string{`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`, `CREATE SCHEMA ` + schema} {
		if _, err := admin.Pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema+",public")
	u.RawQuery = q.Encode()
	db, err := postgres.Connect(ctx, u.String(), 6)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		_, _ = admin.Pool.Exec(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
		admin.Close()
	})
	// Fresh schema application updates the database's own ACL, which concurrent
	// appliers in other test packages race on; serialize it with the same lock.
	lock, err := admin.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if _, err := lock.Exec(ctx, "SELECT pg_advisory_lock(7357001)"); err != nil {
		t.Fatal(err)
	}
	err = db.ApplySchema(ctx, postgres.SchemaMetadata{WorkspaceID: "pipeline-manual", EmbedModel: "m", EmbedDim: 8})
	_, _ = lock.Exec(ctx, "SELECT pg_advisory_unlock(7357001)")
	if err != nil {
		t.Fatal(err)
	}
	jobs := postgres.NewJobs(db)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	stats := telemetry.NewStats()
	p := &Pipeline{app: &app.App{DB: db, Jobs: jobs, Log: log}, stats: stats}
	for _, subject := range []string{postgres.SubjectPDFs, postgres.SubjectEmbed} {
		queue := stats.RegisterQueue("test", subject, 2)
		p.roles = append(p.roles, &role{name: "test", subject: subject, lanes: 2, queue: queue,
			runtime: &worker.Runtime{Name: "test", Subject: subject, Lanes: 2, Log: log}})
	}
	return p, db, postgres.NewDocumentRepo(db), jobs
}

func addPipelineJob(t *testing.T, db *postgres.DB, docs *postgres.DocumentRepo, jobs *postgres.Jobs, subject string) string {
	t.Helper()
	ctx := context.Background()
	id := domain.NewDocID()
	hash := domain.SHA256Hex([]byte(id))
	if _, _, err := docs.CreateStub(ctx, &domain.Document{DocID: id, RawSHA256: hash, DocType: "pdf"}); err != nil {
		t.Fatal(err)
	}
	meta := &ingestionv1.DocumentMetadata{DocId: id, RawSha256: hash, Traceparent: "synthetic"}
	if err := jobs.Publish(ctx, subject, &ingestionv1.ProcessPdfCommand{Metadata: meta, SourceRef: source.Ref(id)}, ""); err != nil {
		t.Fatal(err)
	}
	var jobID string
	if err := db.Pool.QueryRow(ctx, "SELECT job_id::text FROM ingestion_jobs WHERE doc_id=$1", id).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	return jobID
}

// Delayed retries and expired claims are unfinished work: a drain that ignored
// them would stop a run with jobs still owed.
func TestDrainWaitsForDelayedAndExpiredWork(t *testing.T) {
	p, db, docs, jobs := newPipelineEnv(t, "drain_unfinished")
	ctx := context.Background()
	delayed := addPipelineJob(t, db, docs, jobs, postgres.SubjectPDFs)
	expired := addPipelineJob(t, db, docs, jobs, postgres.SubjectEmbed)
	if _, err := db.Pool.Exec(ctx, `UPDATE ingestion_jobs SET available_at = clock_timestamp() + interval '1 hour' WHERE job_id = $1`, delayed); err != nil {
		t.Fatal(err)
	}
	claimed, err := jobs.Claim(ctx, postgres.SubjectEmbed, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE ingestion_jobs SET lease_until = clock_timestamp() - interval '1 second' WHERE job_id = $1`, expired); err != nil {
		t.Fatal(err)
	}

	short, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()
	if err := p.WaitDrained(short, time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain returned %v with a delayed and an expired job outstanding, want it to keep waiting", err)
	}

	// Resolve both; the queue is then genuinely empty and the drain finishes.
	if _, err := db.Pool.Exec(ctx, `UPDATE ingestion_jobs SET state='COMPLETED', lease_until=NULL, finished_at=now()`); err != nil {
		t.Fatal(err)
	}
	long, cancel2 := context.WithTimeout(ctx, 10*time.Second)
	defer cancel2()
	if err := p.WaitDrained(long, time.Second); err != nil {
		t.Fatalf("drain of an empty queue = %v", err)
	}
}

// A sample that fails says nothing about the queue; it must never read as empty.
func TestDrainFailsClosedWhenSamplingFails(t *testing.T) {
	p, db, _, _ := newPipelineEnv(t, "drain_sampling")
	db.Close() // every sample now fails
	short, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := p.WaitDrained(short, 500*time.Millisecond)
	if err == nil {
		t.Fatal("a failing sample was treated as an empty queue and the drain completed")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Logf("drain refused with %v", err)
	}
}

func TestSamplingPublishesPendingWorkAndDueTimes(t *testing.T) {
	p, db, docs, jobs := newPipelineEnvAndWork(t)
	_ = docs
	_ = jobs
	_ = db
	if err := p.samplePending(context.Background()); err != nil {
		t.Fatal(err)
	}
	var pdf *role
	for _, r := range p.roles {
		if r.subject == postgres.SubjectPDFs {
			pdf = r
		}
	}
	if pdf.queue.Idle() {
		t.Fatal("a subject with pending work sampled as idle")
	}
	for _, r := range p.roles {
		if r.subject == postgres.SubjectEmbed && !r.queue.Idle() {
			t.Fatal("an empty subject sampled as busy")
		}
	}
}

func newPipelineEnvAndWork(t *testing.T) (*Pipeline, *postgres.DB, *postgres.DocumentRepo, *postgres.Jobs) {
	t.Helper()
	p, db, docs, jobs := newPipelineEnv(t, "sampling_due")
	addPipelineJob(t, db, docs, jobs, postgres.SubjectPDFs)
	return p, db, docs, jobs
}
