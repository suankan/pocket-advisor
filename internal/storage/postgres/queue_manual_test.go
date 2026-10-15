//go:build manual

package postgres

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ingestionv1 "github.com/suankan/pocket-advisor/api/proto/v1/gen"
	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/source"
	"google.golang.org/protobuf/proto"
)

// Database-backed rehearsal of the PostgreSQL job queue. Manual because it
// needs a live PostgreSQL with pgvector; the default suite stays hermetic.
//
//	Run: EMAIL_DSN=postgres://<role>@localhost:5432/<disposable-db> \
//		go test -tags manual ./internal/storage/postgres/ -run Queue -v
//
// Every test builds its own scratch schema (see scratch) and uses synthetic
// content only. Point EMAIL_DSN at a disposable database, never a workspace.

// roleDSN points a DSN at another database as another role.
func roleDSN(dsn, role, database string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		panic(err)
	}
	u.User = url.User(role)
	u.Path = "/" + database
	return u.String()
}

// schemaApplyLock serializes fresh schema application across test packages
// that share one disposable database: the fresh path updates the database's own
// ACL, which concurrent appliers race on ("tuple concurrently updated"). Real
// deployments have one process per workspace database, so this is a harness
// concern only.
const schemaApplyLock = 7357001

func applySchemaSerialized(t *testing.T, db *DB, meta SchemaMetadata) {
	t.Helper()
	ctx := context.Background()
	c, err := db.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release()
	if _, err := c.Exec(ctx, "SELECT pg_advisory_lock($1)", schemaApplyLock); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = c.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", schemaApplyLock) }()
	if err := db.ApplySchema(ctx, meta); err != nil {
		t.Fatal(err)
	}
}

func queueSetup(t *testing.T) (*DB, *DocumentRepo, *Jobs) {
	t.Helper()
	db, _ := scratch(t, "queue_"+t.Name())
	applySchemaSerialized(t, db, stageBMeta())
	return db, NewDocumentRepo(db), NewJobs(db)
}

// addJob creates a document and enqueues one job for it on subject.
func addJob(t *testing.T, db *DB, docs *DocumentRepo, jobs *Jobs, subject string) (docID, jobID string) {
	t.Helper()
	ctx := context.Background()
	docID = domain.NewDocID()
	hash := domain.SHA256Hex([]byte(docID))
	if _, _, err := docs.CreateStub(ctx, &domain.Document{DocID: docID, RawSHA256: hash, DocType: "pdf"}); err != nil {
		t.Fatal(err)
	}
	meta := &ingestionv1.DocumentMetadata{DocId: docID, RawSha256: hash, Traceparent: "synthetic"}
	var msg proto.Message = &ingestionv1.ProcessPdfCommand{Metadata: meta, SourceRef: source.Ref(docID)}
	if subject == SubjectEmbed {
		msg = &ingestionv1.EmbedTextCommand{Metadata: meta}
	}
	if err := jobs.Publish(ctx, subject, msg, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, "SELECT job_id::text FROM ingestion_jobs WHERE doc_id=$1", docID).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	return docID, jobID
}

type jobRow struct {
	State                   string
	Attempts                int
	Generation              int64
	LeaseSet                bool
	LastFailure, Reason     string
	AvailableInFuture       bool
	FinishedSet, EqualsMade bool
}

func readJob(t *testing.T, db *DB, id string) jobRow {
	t.Helper()
	var r jobRow
	err := db.Pool.QueryRow(context.Background(), `SELECT state, attempts, generation, lease_until IS NOT NULL,
 last_failure_reason, reason, available_at > clock_timestamp() + interval '1 second',
 finished_at IS NOT NULL, available_at = created_at FROM ingestion_jobs WHERE job_id = $1`, id).
		Scan(&r.State, &r.Attempts, &r.Generation, &r.LeaseSet, &r.LastFailure, &r.Reason, &r.AvailableInFuture, &r.FinishedSet, &r.EqualsMade)
	if err != nil {
		t.Fatalf("read job %s: %v", id, err)
	}
	return r
}

func expire(t *testing.T, db *DB, id string) {
	t.Helper()
	if _, err := db.Pool.Exec(context.Background(),
		"UPDATE ingestion_jobs SET lease_until = clock_timestamp() - interval '1 second' WHERE job_id = $1", id); err != nil {
		t.Fatal(err)
	}
}

func claimOne(t *testing.T, jobs *Jobs, subject string) Job {
	t.Helper()
	got, err := jobs.Claim(context.Background(), subject, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("claimed %d jobs, want 1", len(got))
	}
	return got[0]
}

func docStatus(t *testing.T, db *DB, docID string) string {
	t.Helper()
	var s string
	if err := db.Pool.QueryRow(context.Background(), "SELECT processing_status::text FROM documents WHERE doc_id=$1", docID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// downgradeToBrokerLayout rewrites ingestion_jobs into the layout that existed
// before the PostgreSQL queue, keeping its rows.
func downgradeToBrokerLayout(t *testing.T, db *DB) {
	t.Helper()
	for _, sql := range []string{
		`ALTER TABLE ingestion_jobs DROP CONSTRAINT ingestion_jobs_state_check, DROP CONSTRAINT ingestion_jobs_attempts_check, DROP CONSTRAINT ingestion_jobs_lease_check`,
		// Work being handled had no equivalent: it was dispatched.
		`UPDATE ingestion_jobs SET state = 'COMPLETED', lease_until = NULL, finished_at = now() WHERE state = 'RUNNING'`,
		`DROP INDEX ingestion_jobs_claim_ready_idx`,
		`DROP INDEX ingestion_jobs_claim_expired_idx`,
		`ALTER TABLE ingestion_jobs DROP COLUMN attempts, DROP COLUMN lease_until, DROP COLUMN available_at, DROP COLUMN claimed_at, DROP COLUMN claimed_by, DROP COLUMN last_failure_reason`,
		`ALTER TABLE ingestion_jobs ADD COLUMN dispatched_at timestamptz, ADD COLUMN dispatch_lease_until timestamptz`,
		`ALTER TABLE ingestion_jobs ADD CONSTRAINT ingestion_jobs_state_check CHECK(state IN ('READY','DISPATCHED','COMPLETED','FAILED','WAITING_SOURCE'))`,
		`CREATE INDEX ingestion_jobs_ready_idx ON ingestion_jobs(created_at) WHERE state='READY'`,
		// A real workspace already carries rows in this table from earlier
		// releases, with numbers this release must not trust: leave version 2
		// and 3 present, as one such workspace has.
		`INSERT INTO application_migrations(version) VALUES (2), (3) ON CONFLICT DO NOTHING`,
	} {
		if _, err := db.Pool.Exec(context.Background(), sql); err != nil {
			t.Fatalf("downgrade %q: %v", sql, err)
		}
	}
}

func TestQueueSchemaFreshLayoutAndIndexes(t *testing.T) {
	db, _ := scratch(t, "queue_schema_fresh")
	ctx := context.Background()
	if ready, err := db.QueueSchemaReady(ctx); err != nil || ready {
		t.Fatalf("an empty database reported ready: %v %v", ready, err)
	}
	applySchemaSerialized(t, db, stageBMeta())
	if ready, err := db.QueueSchemaReady(ctx); err != nil || !ready {
		t.Fatalf("fresh database not ready: %v %v", ready, err)
	}
	var schema string
	if err := db.Pool.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]string{
		"ingestion_jobs_claim_ready_idx":   {"(subject, available_at, created_at, job_id)", "state = 'READY'"},
		"ingestion_jobs_claim_expired_idx": {"(subject, lease_until, created_at, job_id)", "state = 'RUNNING'"},
	} {
		var def string
		if err := db.Pool.QueryRow(ctx, "SELECT indexdef FROM pg_indexes WHERE schemaname=$1 AND indexname=$2", schema, name).Scan(&def); err != nil {
			t.Fatalf("index %s: %v", name, err)
		}
		for _, frag := range want {
			if !strings.Contains(def, frag) {
				t.Errorf("index %s = %q, want fragment %q", name, def, frag)
			}
		}
	}
	if indexExists(t, db, schema, "ingestion_jobs_ready_idx") {
		t.Error("the old created_at-only ready index was kept")
	}
	for _, col := range []string{"dispatched_at", "dispatch_lease_until"} {
		var n int
		_ = db.Pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
 WHERE table_schema=$1 AND table_name='ingestion_jobs' AND column_name=$2`, schema, col).Scan(&n)
		if n != 0 {
			t.Errorf("column %s survived", col)
		}
	}
}

func TestQueueSchemaUpgradePreservesWorkAndIsRepeatable(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	_, ready := addJob(t, db, docs, jobs, SubjectPDFs)
	_, dispatched := addJob(t, db, docs, jobs, SubjectPDFs)
	_, completed := addJob(t, db, docs, jobs, SubjectPDFs)
	_, failed := addJob(t, db, docs, jobs, SubjectPDFs)
	_, sourceFailed := addJob(t, db, docs, jobs, SubjectPDFs)
	_, waiting := addJob(t, db, docs, jobs, SubjectPDFs)

	downgradeToBrokerLayout(t, db)
	if ok, err := db.QueueSchemaReady(ctx); err != nil || ok {
		t.Fatalf("the broker layout reported ready: %v %v", ok, err)
	}
	// The workspace already carries migration rows numbered like this release's
	// (an earlier release wrote them). The upgrade must not mistake them for
	// its own: it reads the table, not the number.
	var rows int
	if err := db.Pool.QueryRow(ctx, "SELECT count(*) FROM application_migrations WHERE version IN (2, 3)").Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("the colliding migration rows are not in place: %d %v", rows, err)
	}
	for _, sql := range []string{
		`UPDATE ingestion_jobs SET state='DISPATCHED', generation=5, dispatched_at=now(), dispatch_lease_until=now() WHERE job_id='` + dispatched + `'`,
		`UPDATE ingestion_jobs SET state='COMPLETED', finished_at=now() WHERE job_id='` + completed + `'`,
		`UPDATE ingestion_jobs SET state='FAILED', reason='OCR_FAILED', finished_at=now() WHERE job_id='` + failed + `'`,
		`UPDATE ingestion_jobs SET state='FAILED', reason='SOURCE_CHANGED', finished_at=now() WHERE job_id='` + sourceFailed + `'`,
		`UPDATE ingestion_jobs SET state='WAITING_SOURCE', reason='SOURCE_UNAVAILABLE' WHERE job_id='` + waiting + `'`,
	} {
		if _, err := db.Pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}

	if err := db.ApplySourceSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.QueueSchemaReady(ctx); err != nil || !ok {
		t.Fatalf("upgrade did not reach the queue layout: %v %v", ok, err)
	}
	if r := readJob(t, db, dispatched); r.State != "READY" || r.Generation != 6 || r.Attempts != 0 || r.LeaseSet {
		t.Errorf("DISPATCHED work = %+v, want READY at generation 6 with no attempts or lease", r)
	}
	if r := readJob(t, db, ready); r.State != "READY" || r.Generation != 1 || !r.EqualsMade {
		t.Errorf("READY work = %+v, want unchanged with available_at backfilled from created_at", r)
	}
	if r := readJob(t, db, completed); r.State != "COMPLETED" || !r.FinishedSet {
		t.Errorf("COMPLETED work = %+v", r)
	}
	if r := readJob(t, db, failed); r.State != "FAILED" || r.LastFailure != "OCR_FAILED" || r.Reason != "OCR_FAILED" {
		t.Errorf("FAILED work = %+v, want its processing reason carried into last_failure_reason", r)
	}
	if r := readJob(t, db, sourceFailed); r.LastFailure != "" {
		t.Errorf("a source diagnostic became a processing failure: %+v", r)
	}
	if r := readJob(t, db, waiting); r.State != "WAITING_SOURCE" || r.Reason != "SOURCE_UNAVAILABLE" {
		t.Errorf("source wait = %+v", r)
	}

	// Reapplication must not reset attempts, generations or live leases.
	claimed := claimOne(t, jobs, SubjectPDFs)
	before := readJob(t, db, claimed.ID)
	if err := db.ApplySourceSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if after := readJob(t, db, claimed.ID); after != before {
		t.Errorf("reapplication changed a live claim: %+v -> %+v", before, after)
	}
}

func TestQueueSchemaStepRollsBackAsAWhole(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	_, id := addJob(t, db, docs, jobs, SubjectPDFs)
	downgradeToBrokerLayout(t, db)
	if _, err := db.Pool.Exec(ctx, `UPDATE ingestion_jobs SET state='DISPATCHED' WHERE job_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected failure after the step")
	err := db.Transaction(ctx, func(ctx context.Context) error {
		if err := db.applyQueueSchema(ctx); err != nil {
			return err
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("unexpected result %v", err)
	}
	if ok, _ := db.QueueSchemaReady(ctx); ok {
		t.Fatal("a failed step left the queue layout half-applied")
	}
	var state string
	if err := db.Pool.QueryRow(ctx, "SELECT state FROM ingestion_jobs WHERE job_id=$1", id).Scan(&state); err != nil || state != "DISPATCHED" {
		t.Fatalf("rows changed under a rolled-back step: %q %v", state, err)
	}
}

func TestQueueClaimConcurrentNoDuplicates(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	const total = 60
	want := map[string]bool{}
	for i := 0; i < total; i++ {
		_, id := addJob(t, db, docs, jobs, SubjectPDFs)
		want[id] = true
	}
	var mu sync.Mutex
	got := map[string]int{}
	var wg sync.WaitGroup
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			worker := NewJobs(db) // its own owner, as another claimant would have
			for {
				claimed, err := worker.Claim(context.Background(), SubjectPDFs, 5)
				if err != nil {
					t.Error(err)
					return
				}
				if len(claimed) == 0 {
					return
				}
				mu.Lock()
				for _, j := range claimed {
					got[j.ID]++
					if j.Generation != 2 || j.Attempts != 1 || !j.Charged || j.LeaseUntil.IsZero() {
						t.Errorf("claim = %+v", j)
					}
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(got) != total {
		t.Fatalf("claimed %d of %d jobs", len(got), total)
	}
	for id, n := range got {
		if n != 1 || !want[id] {
			t.Fatalf("job %s claimed %d times", id, n)
		}
	}
}

func TestQueueClaimOrdersByAvailabilityThenCreation(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	_, late := addJob(t, db, docs, jobs, SubjectPDFs)
	_, second := addJob(t, db, docs, jobs, SubjectPDFs)
	_, first := addJob(t, db, docs, jobs, SubjectPDFs)
	// Equal availability for first and second; first was created earlier.
	for _, sql := range []string{
		`UPDATE ingestion_jobs SET available_at = clock_timestamp() - interval '10 seconds', created_at = clock_timestamp() - interval '30 seconds' WHERE job_id = '` + first + `'`,
		`UPDATE ingestion_jobs SET available_at = (SELECT available_at FROM ingestion_jobs WHERE job_id = '` + first + `'), created_at = clock_timestamp() - interval '20 seconds' WHERE job_id = '` + second + `'`,
		`UPDATE ingestion_jobs SET available_at = clock_timestamp() - interval '1 second' WHERE job_id = '` + late + `'`,
	} {
		if _, err := db.Pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	var order []string
	for i := 0; i < 3; i++ {
		order = append(order, claimOne(t, jobs, SubjectPDFs).ID)
	}
	if order[0] != first || order[1] != second || order[2] != late {
		t.Fatalf("claim order %v, want [%s %s %s]", order, first, second, late)
	}
}

func TestQueueClaimWaitsOutADelayAndScopesBySubject(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	_, delayed := addJob(t, db, docs, jobs, SubjectPDFs)
	_, other := addJob(t, db, docs, jobs, SubjectEmbed)
	if _, err := db.Pool.Exec(ctx, `UPDATE ingestion_jobs SET available_at = clock_timestamp() + interval '1 hour' WHERE job_id = $1`, delayed); err != nil {
		t.Fatal(err)
	}
	if got, err := jobs.Claim(ctx, SubjectPDFs, 5); err != nil || len(got) != 0 {
		t.Fatalf("a delayed job was claimed early: %v %v", got, err)
	}
	if got := claimOne(t, jobs, SubjectEmbed); got.ID != other {
		t.Fatalf("claimed %s, want the embed job %s", got.ID, other)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE ingestion_jobs SET available_at = clock_timestamp() - interval '1 second' WHERE job_id = $1`, delayed); err != nil {
		t.Fatal(err)
	}
	if got := claimOne(t, jobs, SubjectPDFs); got.ID != delayed {
		t.Fatalf("claimed %s once due, want %s", got.ID, delayed)
	}
}

func TestQueueExpiredClaimsChargeAttemptsAndExhaust(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	_, id := addJob(t, db, docs, jobs, SubjectPDFs)
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		j := claimOne(t, jobs, SubjectPDFs)
		if j.ID != id || j.Attempts != attempt || !j.Charged || j.Generation != int64(attempt+1) {
			t.Fatalf("claim %d = %+v", attempt, j)
		}
		expire(t, db, id) // the worker died
	}
	// The third attempt crashed. The next claim must authorize failure
	// bookkeeping only, never a fourth handler.
	j := claimOne(t, jobs, SubjectPDFs)
	if j.Charged || j.Attempts != MaxAttempts {
		t.Fatalf("exhausted claim = %+v, want uncharged at %d attempts", j, MaxAttempts)
	}
	if err := jobs.Fail(context.Background(), j, domain.ReasonUnclassified); err != nil {
		t.Fatal(err)
	}
	if r := readJob(t, db, id); r.State != "FAILED" || r.Reason != "UNCLASSIFIED" || r.LeaseSet || !r.FinishedSet {
		t.Fatalf("exhausted job = %+v", r)
	}
	if got, err := jobs.Claim(context.Background(), SubjectPDFs, 3); err != nil || len(got) != 0 {
		t.Fatalf("a failed job was claimed again: %v %v", got, err)
	}
}

func TestQueueNeitherStateStarvesTheOther(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	var expired string
	_, expired = addJob(t, db, docs, jobs, SubjectPDFs)
	first := claimOne(t, jobs, SubjectPDFs)
	expire(t, db, expired)
	for i := 0; i < 4; i++ {
		addJob(t, db, docs, jobs, SubjectPDFs)
	}
	// One reservation: the two states alternate as the first choice.
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		seen[claimOne(t, jobs, SubjectPDFs).ID] = true
	}
	if !seen[first.ID] {
		t.Fatal("a continuous READY backlog starved the expired claim")
	}
	// Several reservations: an expired claim is tried before READY work fills in.
	_, another := addJob(t, db, docs, jobs, SubjectPDFs)
	_ = another
	j := claimOne(t, jobs, SubjectPDFs)
	expire(t, db, j.ID)
	got, err := jobs.Claim(context.Background(), SubjectPDFs, 3)
	if err != nil {
		t.Fatal(err)
	}
	reclaimed := false
	for _, c := range got {
		reclaimed = reclaimed || c.ID == j.ID
	}
	if !reclaimed {
		t.Fatalf("a multi-slot claim ignored the expired job: %+v", got)
	}
}

func explain(t *testing.T, db *DB, statement string, args ...any) string {
	t.Helper()
	rows, err := db.Pool.Query(context.Background(), "EXPLAIN "+statement, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		b.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestQueueClaimPlansFollowTheirIndexesWithoutSorting(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	doc, _ := addJob(t, db, docs, jobs, SubjectPDFs)
	for _, sql := range []string{
		// A large eligible backlog, a large delayed one, and many expired claims.
		`INSERT INTO ingestion_jobs(doc_id,subject,revision,payload,traceparent,available_at)
		 SELECT '` + doc + `', 'ingest.pdfs.raw', 'ready-'||g, '\x00', 'tp', clock_timestamp() - make_interval(secs => g) FROM generate_series(1,4000) g`,
		`INSERT INTO ingestion_jobs(doc_id,subject,revision,payload,traceparent,available_at)
		 SELECT '` + doc + `', 'ingest.pdfs.raw', 'delayed-'||g, '\x00', 'tp', clock_timestamp() + make_interval(secs => 3600 + g) FROM generate_series(1,4000) g`,
		`INSERT INTO ingestion_jobs(doc_id,subject,revision,payload,traceparent,state,claimed_at,lease_until)
		 SELECT '` + doc + `', 'ingest.pdfs.raw', 'expired-'||g, '\x00', 'tp', 'RUNNING', clock_timestamp() - interval '10 minutes', clock_timestamp() - make_interval(secs => g) FROM generate_series(1,2000) g`,
		`ANALYZE ingestion_jobs`,
	} {
		if _, err := db.Pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	for name, c := range map[string]struct{ statement, index string }{
		"ready":   {claimReadySQL, "ingestion_jobs_claim_ready_idx"},
		"expired": {claimExpiredSQL, "ingestion_jobs_claim_expired_idx"},
	} {
		plan := explain(t, db, c.statement, SubjectPDFs, 5, "owner", LeaseDuration.Seconds())
		t.Logf("%s claim plan:\n%s", name, plan)
		if !strings.Contains(plan, c.index) {
			t.Errorf("%s claim does not use %s:\n%s", name, c.index, plan)
		}
		if strings.Contains(plan, "Sort") {
			t.Errorf("%s claim sorts its eligible backlog instead of following the index:\n%s", name, plan)
		}
	}
	// The claim itself must stay correct at that size.
	got, err := jobs.Claim(ctx, SubjectPDFs, 5)
	if err != nil || len(got) != 5 {
		t.Fatalf("claim at backlog: %d %v", len(got), err)
	}
}

func TestQueueRenewExtendsCapsAndRejectsExpiry(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	_, id := addJob(t, db, docs, jobs, SubjectPDFs)
	j := claimOne(t, jobs, SubjectPDFs)
	if _, err := db.Pool.Exec(ctx, `UPDATE ingestion_jobs SET lease_until = clock_timestamp() + interval '10 seconds' WHERE job_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	l, err := jobs.Renew(ctx, j)
	if err != nil {
		t.Fatal(err)
	}
	if remaining := l.Until.Sub(l.ServerNow); remaining < LeaseDuration-5*time.Second || remaining > LeaseDuration {
		t.Fatalf("renewed lease has %v remaining, want about %v", remaining, LeaseDuration)
	}
	// Near the one-hour cap the lease is trimmed to it, not extended past it.
	if _, err := db.Pool.Exec(ctx, `UPDATE ingestion_jobs SET claimed_at = clock_timestamp() - interval '59 minutes 50 seconds' WHERE job_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	l, err = jobs.Renew(ctx, j)
	if err != nil {
		t.Fatal(err)
	}
	if remaining := l.Until.Sub(l.ServerNow); remaining > 15*time.Second {
		t.Fatalf("lease %v remaining past the handler budget", remaining)
	}
	// Past the cap, renewal is refused even though the lease row is still live.
	if _, err := db.Pool.Exec(ctx, `UPDATE ingestion_jobs SET claimed_at = clock_timestamp() - interval '61 minutes' WHERE job_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.Renew(ctx, j); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("renewal past the budget = %v, want ErrLeaseLost", err)
	}
}

// Expiry alone invalidates ownership: with no successor and no reaper, an
// expired worker still cannot renew, run, or record any verdict.
func TestQueueExpiryRejectsEveryOperationWithoutTakeover(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	_, id := addJob(t, db, docs, jobs, SubjectPDFs)
	j := claimOne(t, jobs, SubjectPDFs)
	expire(t, db, id)
	if _, err := jobs.Renew(ctx, j); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("renew = %v", err)
	}
	ran := false
	if err := jobs.Run(ctx, j, func(context.Context) error { ran = true; return nil }); !errors.Is(err, ErrLeaseLost) || ran {
		t.Errorf("run = %v (handler ran: %v)", err, ran)
	}
	if err := jobs.Retry(ctx, j, time.Second, domain.ReasonUnclassified); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("retry = %v", err)
	}
	if err := jobs.Release(ctx, j, 0); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("release = %v", err)
	}
	if err := jobs.Wait(ctx, j, "SOURCE_UNAVAILABLE"); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("wait = %v", err)
	}
	if err := jobs.Fail(ctx, j, domain.ReasonUnclassified); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("fail = %v", err)
	}
	if r := readJob(t, db, id); r.State != "RUNNING" || r.Attempts != 1 {
		t.Errorf("a refused verdict changed the row: %+v", r)
	}
}

func TestQueueRunCommitsAtomicallyAndRefusesAReplay(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	docID, id := addJob(t, db, docs, jobs, SubjectPDFs)
	j := claimOne(t, jobs, SubjectPDFs)
	handler := func(ctx context.Context) error {
		if err := docs.SaveText(ctx, docID, "canonical text", "pdf"); err != nil {
			return err
		}
		return jobs.Publish(ctx, SubjectEmbed, &ingestionv1.EmbedTextCommand{
			Metadata: &ingestionv1.DocumentMetadata{DocId: docID, RawSha256: domain.SHA256Hex([]byte(docID)), Traceparent: "synthetic"}}, "")
	}
	crash := errors.New("injected interruption")
	if err := jobs.Run(ctx, j, func(ctx context.Context) error {
		if err := handler(ctx); err != nil {
			return err
		}
		return crash
	}); !errors.Is(err, crash) {
		t.Fatal(err)
	}
	var text *string
	if err := db.Pool.QueryRow(ctx, "SELECT normalized_text FROM documents WHERE doc_id=$1", docID).Scan(&text); err != nil || text != nil {
		t.Fatalf("stage rollback leaked text %v %v", text, err)
	}
	if ready, _, _, _, err := jobs.Counts(ctx); err != nil || ready != 0 {
		t.Fatalf("a rolled-back handler leaked a downstream job: %d %v", ready, err)
	}
	if r := readJob(t, db, id); r.State != "RUNNING" {
		t.Fatalf("a failed handler changed the job: %+v", r)
	}
	if err := jobs.Run(ctx, j, handler); err != nil {
		t.Fatal(err)
	}
	if r := readJob(t, db, id); r.State != "COMPLETED" || r.LeaseSet || !r.FinishedSet {
		t.Fatalf("completed job = %+v", r)
	}
	if ready, _, _, _, err := jobs.Counts(ctx); err != nil || ready != 1 {
		t.Fatalf("downstream job missing: %d %v", ready, err)
	}
	// The same claim presented again is a stale one.
	if err := jobs.Run(ctx, j, func(context.Context) error { t.Fatal("completed handler repeated"); return nil }); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("replay = %v, want ErrLeaseLost", err)
	}
	if _, err := docs.DeleteDocuments(ctx, []string{docID}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Run(ctx, j, func(context.Context) error { t.Fatal("deleted document resurrected"); return nil }); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("deleted = %v", err)
	}
}

func TestQueueStaleVerdictsLeaveTheSuccessorUntouched(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	docID, id := addJob(t, db, docs, jobs, SubjectPDFs)
	stale := claimOne(t, jobs, SubjectPDFs)
	expire(t, db, id)
	successor := claimOne(t, NewJobs(db), SubjectPDFs)
	if successor.Generation != stale.Generation+1 {
		t.Fatalf("generations %d -> %d", stale.Generation, successor.Generation)
	}
	if err := docs.UpdateStatus(ctx, docID, domain.StatusProcessing, ""); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Retry(ctx, stale, time.Second, domain.ReasonUnclassified); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("stale retry = %v", err)
	}
	if err := jobs.Release(ctx, stale, 0); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("stale release = %v", err)
	}
	if err := jobs.Wait(ctx, stale, "SOURCE_CHANGED"); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("stale wait = %v", err)
	}
	if err := jobs.Fail(ctx, stale, domain.ReasonExtractionFailed); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("stale fail = %v", err)
	}
	if err := jobs.Run(ctx, stale, func(context.Context) error { t.Fatal("stale handler ran"); return nil }); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("stale run = %v", err)
	}
	if got := docStatus(t, db, docID); got != "PROCESSING" {
		t.Errorf("a stale failure verdict changed the document to %s", got)
	}
	if r := readJob(t, db, id); r.State != "RUNNING" || r.Generation != successor.Generation || r.Attempts != 2 || !r.LeaseSet {
		t.Errorf("successor disturbed: %+v", r)
	}
	// The successor's own verdict still works.
	if err := jobs.Run(ctx, successor, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("successor run: %v", err)
	}
}

func TestQueueFenceHoldsWhenTheJobChangesMidHandler(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	for name, mutate := range map[string]string{
		"deleted job":         `DELETE FROM ingestion_jobs WHERE job_id = '%s'`,
		"recovery generation": `UPDATE ingestion_jobs SET generation = generation + 1 WHERE job_id = '%s'`,
		"expired lease":       `UPDATE ingestion_jobs SET lease_until = clock_timestamp() - interval '1 second' WHERE job_id = '%s'`,
	} {
		addJob(t, db, docs, jobs, SubjectPDFs)
		// Cases leave their jobs behind, so the claim may be an earlier case's
		// expired job: every assertion is about the job actually claimed.
		j := claimOne(t, jobs, SubjectPDFs)
		docID, id := j.DocID, j.ID
		err := jobs.Run(ctx, j, func(ctx context.Context) error {
			if err := docs.SaveText(ctx, docID, "stage output that must not commit", "pdf"); err != nil {
				return err
			}
			// Another connection changes the job while the handler works.
			_, err := db.Pool.Exec(context.Background(), fmt.Sprintf(mutate, id))
			return err
		})
		if !errors.Is(err, ErrLeaseLost) {
			t.Errorf("%s: run = %v, want ErrLeaseLost", name, err)
		}
		var text *string
		if err := db.Pool.QueryRow(ctx, "SELECT normalized_text FROM documents WHERE doc_id=$1", docID).Scan(&text); err != nil || text != nil {
			t.Errorf("%s: stage writes survived the failed fence: %v %v", name, text, err)
		}
	}
}

func TestQueueBusyAdvisoryLockRefundsOnlyAChargedClaim(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	_, id := addJob(t, db, docs, jobs, SubjectPDFs)
	stale := claimOne(t, jobs, SubjectPDFs)

	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- jobs.Run(ctx, stale, func(context.Context) error { close(entered); <-release; return nil })
	}()
	<-entered
	// Its lease expires while it is still physically running and holds the
	// job's advisory lock; a successor claims the job.
	expire(t, db, id)
	other := NewJobs(db)
	successor := claimOne(t, other, SubjectPDFs)
	if successor.Attempts != 2 || !successor.Charged {
		t.Fatalf("successor = %+v", successor)
	}
	if err := other.Run(ctx, successor, func(context.Context) error { t.Error("duplicate handler ran"); return nil }); !errors.Is(err, ErrJobBusy) {
		t.Fatalf("duplicate run = %v, want ErrJobBusy", err)
	}
	if err := other.Release(ctx, successor, BusyDelay); err != nil {
		t.Fatal(err)
	}
	if r := readJob(t, db, id); r.State != "READY" || r.Attempts != 1 || !r.AvailableInFuture || r.LeaseSet {
		t.Fatalf("busy return = %+v, want READY later with the charged attempt refunded", r)
	}
	close(release)
	if err := <-done; !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("the stale handler's completion = %v, want ErrLeaseLost", err)
	}
}

func TestQueueRefundsOccurExactlyOnceAndSourceWaitsConsumeNoAttempt(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	_, id := addJob(t, db, docs, jobs, SubjectPDFs)

	j := claimOne(t, jobs, SubjectPDFs)
	if err := jobs.Release(ctx, j, 0); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Release(ctx, j, 0); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("a repeated release = %v, want ErrLeaseLost", err)
	}
	if r := readJob(t, db, id); r.State != "READY" || r.Attempts != 0 {
		t.Fatalf("after shutdown return: %+v", r)
	}

	// A transient failure keeps its attempt and records its reason.
	j = claimOne(t, jobs, SubjectPDFs)
	if err := jobs.Retry(ctx, j, 0, domain.ReasonExtractionFailed); err != nil {
		t.Fatal(err)
	}
	if r := readJob(t, db, id); r.State != "READY" || r.Attempts != 1 || r.LastFailure != "EXTRACTION_FAILED" {
		t.Fatalf("after retry: %+v", r)
	}

	// A source wait refunds its charge and keeps the earlier failure.
	j = claimOne(t, jobs, SubjectPDFs)
	if j.Attempts != 2 {
		t.Fatalf("attempts = %d", j.Attempts)
	}
	if err := jobs.Wait(ctx, j, "SOURCE_UNAVAILABLE"); err != nil {
		t.Fatal(err)
	}
	if r := readJob(t, db, id); r.State != "WAITING_SOURCE" || r.Attempts != 1 || r.Reason != "SOURCE_UNAVAILABLE" || r.LastFailure != "EXTRACTION_FAILED" || r.LeaseSet {
		t.Fatalf("after source wait: %+v", r)
	}
	if err := jobs.CheckResolved(ctx, time.Time{}); err == nil {
		t.Fatal("a missing source counted as success")
	}
	if got, err := jobs.Claim(ctx, SubjectPDFs, 1); err != nil || len(got) != 0 {
		t.Fatalf("a source wait was claimed: %v %v", got, err)
	}
	// Resumption retains the charge and makes the job claimable at once.
	if err := jobs.ResumeSources(ctx); err != nil {
		t.Fatal(err)
	}
	if r := readJob(t, db, id); r.State != "READY" || r.Attempts != 1 || r.Generation != j.Generation+1 {
		t.Fatalf("after resume: %+v", r)
	}
	again := claimOne(t, jobs, SubjectPDFs)
	if again.Attempts != 2 {
		t.Fatalf("resumed claim = %+v", again)
	}
}

func TestQueueFailIsAtomicWithTheDocument(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	before, err := jobs.Clock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	docID, id := addJob(t, db, docs, jobs, SubjectPDFs)
	j := claimOne(t, jobs, SubjectPDFs)
	if err := jobs.Fail(ctx, j, domain.ReasonMalformedCommand); err != nil {
		t.Fatal(err)
	}
	if r := readJob(t, db, id); r.State != "FAILED" || r.Reason != "MALFORMED_COMMAND" || r.LastFailure != "MALFORMED_COMMAND" || r.LeaseSet || !r.FinishedSet {
		t.Fatalf("failed job = %+v", r)
	}
	if got := docStatus(t, db, docID); got != "FAILED" {
		t.Fatalf("document status = %s", got)
	}
	if err := jobs.CheckResolved(ctx, before); err == nil {
		t.Fatal("a job failed during the run counted as resolved")
	}
	// A later run reports the failure without being judged by it: nothing it
	// does retries a failed job.
	after, err := jobs.Clock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.CheckResolved(ctx, after); err != nil {
		t.Fatalf("an earlier failure decided a later run: %v", err)
	}
}

func TestQueueDepthsAndCounts(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	_, delayed := addJob(t, db, docs, jobs, SubjectPDFs)
	addJob(t, db, docs, jobs, SubjectPDFs)
	_, embed := addJob(t, db, docs, jobs, SubjectEmbed)
	if _, err := db.Pool.Exec(ctx, `UPDATE ingestion_jobs SET available_at = clock_timestamp() + interval '1 hour' WHERE job_id = $1`, delayed); err != nil {
		t.Fatal(err)
	}
	running := claimOne(t, jobs, SubjectEmbed)
	if running.ID != embed {
		t.Fatalf("claimed %s", running.ID)
	}
	depths, err := jobs.Depths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pdf, emb := depths[SubjectPDFs], depths[SubjectEmbed]
	if pdf.Pending != 2 || pdf.NextReady.IsZero() || !pdf.NextExpiry.IsZero() {
		t.Errorf("pdf depth = %+v", pdf)
	}
	if emb.Pending != 1 || emb.NextExpiry.IsZero() || !emb.NextReady.IsZero() {
		t.Errorf("embed depth = %+v", emb)
	}
	if !emb.NextExpiry.After(emb.ServerNow) {
		t.Errorf("a live lease reports an expiry in the past: %+v", emb)
	}
	ready, unfinished, waiting, failed, err := jobs.Counts(ctx)
	if err != nil || ready != 2 || unfinished != 3 || waiting != 0 || failed != 0 {
		t.Errorf("counts = %d %d %d %d %v", ready, unfinished, waiting, failed, err)
	}
}

func TestQueueListenerWakesOnCommitAndReconnects(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	var wakes atomic.Int64
	l := db.NewListener(func() { wakes.Add(1) }, func(msg string, args ...any) { t.Log(msg, args) })
	l.Start(ctx)
	defer l.Close()
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// A job published inside an open transaction is invisible until commit,
	// and so is its notification.
	before := wakes.Load()
	docID := domain.NewDocID()
	hash := domain.SHA256Hex([]byte(docID))
	if _, _, err := docs.CreateStub(ctx, &domain.Document{DocID: docID, RawSHA256: hash, DocType: "pdf"}); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- db.Transaction(ctx, func(ctx context.Context) error {
			err := jobs.Publish(ctx, SubjectPDFs, &ingestionv1.ProcessPdfCommand{
				Metadata: &ingestionv1.DocumentMetadata{DocId: docID, RawSha256: hash, Traceparent: "synthetic"}, SourceRef: source.Ref(docID)}, "")
			<-release
			return err
		})
	}()
	time.Sleep(300 * time.Millisecond)
	if got := wakes.Load(); got != before {
		t.Fatalf("a notification arrived before its transaction committed (%d -> %d)", before, got)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	waitFor("the commit notification", func() bool { return wakes.Load() > before })

	// Kill the listening backend; the listener must reconnect and re-LISTEN.
	var killed int
	if err := db.Pool.QueryRow(ctx, `SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity
 WHERE datname = current_database() AND query LIKE 'LISTEN %' AND pid <> pg_backend_pid()`).Scan(&killed); err != nil || killed == 0 {
		t.Fatalf("no listener backend to terminate: %d %v", killed, err)
	}
	mark := wakes.Load()
	waitFor("the reconnect wake", func() bool { return wakes.Load() > mark })
	mark = wakes.Load()
	addJob(t, db, docs, jobs, SubjectPDFs)
	waitFor("a notification after reconnecting", func() bool { return wakes.Load() > mark })
}

func TestQueueRetryAndReleaseNotifyAtCommit(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	addJob(t, db, docs, jobs, SubjectPDFs)
	j := claimOne(t, jobs, SubjectPDFs)
	var wakes atomic.Int64
	l := db.NewListener(func() { wakes.Add(1) }, nil)
	l.Start(ctx)
	defer l.Close()
	time.Sleep(200 * time.Millisecond)
	before := wakes.Load()
	if err := jobs.Retry(ctx, j, time.Minute, domain.ReasonUnclassified); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for wakes.Load() == before {
		if time.Now().After(deadline) {
			t.Fatal("a retry committed without notifying")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The maintenance lock lives on one dedicated session. When it drops, a new
// process can take the lock while an old handler's transaction is still open,
// so leases and fences — not the lock — must keep the job safe.
func TestQueueMaintenanceSessionLossKeepsLeasesAndFences(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	_, id := addJob(t, db, docs, jobs, SubjectPDFs)
	unlock, err := db.LockMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	j := claimOne(t, jobs, SubjectPDFs)

	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- jobs.Run(ctx, j, func(context.Context) error { close(entered); <-release; return nil })
	}()
	<-entered
	var killed int
	if err := db.Pool.QueryRow(ctx, `SELECT count(pg_terminate_backend(l.pid)) FROM pg_locks l
 WHERE l.locktype = 'advisory' AND l.granted AND l.pid <> pg_backend_pid()
   AND l.pid IN (SELECT pid FROM pg_stat_activity WHERE query LIKE '%source-ingestion-maintenance%')`).Scan(&killed); err != nil || killed == 0 {
		t.Fatalf("could not drop the maintenance session: %d %v", killed, err)
	}
	// Another process now acquires the lock while the first handler still runs.
	var second func()
	deadline := time.Now().Add(5 * time.Second)
	for {
		second, err = db.LockMaintenance(ctx)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lock never became available: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer second()
	// Acquiring the lock reset nothing: the claim, its lease and its handler
	// are intact, and the handler completes.
	if r := readJob(t, db, id); r.State != "RUNNING" || !r.LeaseSet {
		t.Fatalf("job state after losing the maintenance session: %+v", r)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("handler = %v, want completion despite the lock change", err)
	}
	if r := readJob(t, db, id); r.State != "COMPLETED" {
		t.Fatalf("job = %+v", r)
	}
}

// A handler whose connection is cut mid-transaction leaves no effects and a
// still-RUNNING row, which its lease then resolves.
func TestQueueHandlerConnectionLossLeavesAnExpirableClaim(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	docID, id := addJob(t, db, docs, jobs, SubjectPDFs)
	j := claimOne(t, jobs, SubjectPDFs)
	err := jobs.Run(ctx, j, func(ctx context.Context) error {
		if err := docs.SaveText(ctx, docID, "never committed", "pdf"); err != nil {
			return err
		}
		_, err := db.executor(ctx).Exec(ctx, "SELECT pg_terminate_backend(pg_backend_pid())")
		return err
	})
	if err == nil {
		t.Fatal("a terminated connection reported success")
	}
	var text *string
	if err := db.Pool.QueryRow(ctx, "SELECT normalized_text FROM documents WHERE doc_id=$1", docID).Scan(&text); err != nil || text != nil {
		t.Fatalf("effects survived a cut connection: %v %v", text, err)
	}
	if r := readJob(t, db, id); r.State != "RUNNING" || r.Attempts != 1 {
		t.Fatalf("job = %+v, want RUNNING with its attempt charged", r)
	}
	expire(t, db, id)
	if again := claimOne(t, jobs, SubjectPDFs); again.ID != id || again.Attempts != 2 {
		t.Fatalf("reclaim = %+v", again)
	}
}

// With the pool at its minimum of four connections, a full gate must still
// leave room for the maintenance lock, claims, renewals, depth samples and
// verdicts.
func TestQueueMinimumPoolSizeFullGate(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	if db.Pool.Config().MaxConns != 4 {
		t.Fatalf("pool size %d, want the minimum of 4", db.Pool.Config().MaxConns)
	}
	if size := jobs.Gate().Size(); size != 1 {
		t.Fatalf("gate size %d at 4 connections, want 1", size)
	}
	unlock, err := db.LockMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	addJob(t, db, docs, jobs, SubjectPDFs)
	addJob(t, db, docs, jobs, SubjectEmbed)
	running := claimOne(t, jobs, SubjectPDFs)
	if jobs.Gate().TryReserve(1) != 1 {
		t.Fatal("could not fill the gate")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- jobs.Run(ctx, running, func(context.Context) error { close(entered); <-release; return nil })
	}()
	<-entered

	// The lock holds one connection and the open handler another. Everything
	// else must fit in the remaining two, each inside its control deadline.
	control := make(chan error, 4)
	start := time.Now()
	go func() { _, err := jobs.Renew(ctx, running); control <- err }()
	go func() { _, err := jobs.Depths(ctx); control <- err }()
	go func() { _, err := jobs.Claim(ctx, SubjectEmbed, 1); control <- err }()
	go func() { _, _, _, _, err := jobs.Counts(ctx); control <- err }()
	for i := 0; i < 4; i++ {
		if err := <-control; err != nil {
			t.Fatalf("control query %d: %v", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed > ControlTimeout {
		t.Fatalf("control queries took %v under a full gate", elapsed)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	jobs.Gate().Release(1)
}

// A role owns its own workspace database and no other. The queue lives in that
// database, so confinement is the database's own: another role cannot connect,
// read, claim, renew or mutate. (Under trust authentication this proves
// privilege confinement, not authentication.)
func TestQueueRolesCannotReachEachOthersJobs(t *testing.T) {
	adminDSN := os.Getenv("EMAIL_DSN")
	if adminDSN == "" {
		t.Skip("set EMAIL_DSN to a disposable database with superuser rights")
	}
	ctx := context.Background()
	admin, err := Connect(ctx, adminDSN, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	names := [2]string{"queue_iso_a", "queue_iso_b"}
	for _, n := range names {
		for _, sql := range []string{
			fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, n),
			fmt.Sprintf(`DROP ROLE IF EXISTS %s`, n),
			fmt.Sprintf(`CREATE ROLE %s LOGIN`, n),
			fmt.Sprintf(`CREATE DATABASE %s OWNER %s`, n, n),
		} {
			if _, err := admin.Pool.Exec(ctx, sql); err != nil {
				t.Skipf("cannot create synthetic roles (%v)", err)
			}
		}
	}
	defer func() {
		for _, n := range names {
			_, _ = admin.Pool.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, n))
			_, _ = admin.Pool.Exec(ctx, fmt.Sprintf(`DROP ROLE IF EXISTS %s`, n))
		}
	}()
	dsnFor := func(role, database string) string { return roleDSN(adminDSN, role, database) }
	jobsOf := map[string]*Jobs{}
	dbs := map[string]*DB{}
	ids := map[string]string{}
	for _, n := range names {
		// The extensions need superuser rights, so the admin creates them.
		setup, err := Connect(ctx, roleDSN(adminDSN, "postgres", n), 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, sql := range []string{`CREATE EXTENSION IF NOT EXISTS vector`, `CREATE EXTENSION IF NOT EXISTS pg_textsearch`} {
			if _, err := setup.Pool.Exec(ctx, sql); err != nil {
				t.Skipf("extension unavailable: %v", err)
			}
		}
		setup.Close()
		db, err := Connect(ctx, dsnFor(n, n), 4)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if err := db.ApplySchema(ctx, SchemaMetadata{WorkspaceID: n, EmbedModel: "iso", EmbedDim: stageBDim}); err != nil {
			t.Fatal(err)
		}
		dbs[n], jobsOf[n] = db, NewJobs(db)
		_, ids[n] = addJob(t, db, NewDocumentRepo(db), jobsOf[n], SubjectPDFs)
	}
	a, b := names[0], names[1]
	// Role B cannot even connect to A's database.
	if other, err := Connect(ctx, dsnFor(b, a), 2); err == nil {
		other.Close()
		t.Fatal("a role connected to another workspace's database")
	}
	// Each claims only its own job; the other's row is not visible.
	claimed := claimOne(t, jobsOf[a], SubjectPDFs)
	if claimed.ID != ids[a] {
		t.Fatalf("role A claimed %s, want its own %s", claimed.ID, ids[a])
	}
	if got, err := jobsOf[a].Claim(ctx, SubjectPDFs, 5); err != nil || len(got) != 0 {
		t.Fatalf("role A saw more than its own job: %v %v", got, err)
	}
	if _, err := jobsOf[b].Renew(ctx, claimed); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("role B renewed role A's claim: %v", err)
	}
	if err := jobsOf[b].Fail(ctx, claimed, domain.ReasonUnclassified); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("role B failed role A's job: %v", err)
	}
	if r := readJob(t, dbs[a], ids[a]); r.State != "RUNNING" {
		t.Fatalf("role A's job disturbed: %+v", r)
	}
	if r := readJob(t, dbs[b], ids[b]); r.State != "READY" {
		t.Fatalf("role B's job disturbed: %+v", r)
	}
}

func TestQueueRecoveryResetRevalidatesAndStartsAFreshCycle(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	q := NewDoctorQueries(db)

	// A failed document whose job spent every attempt.
	docID, id := addJob(t, db, docs, jobs, SubjectPDFs)
	if _, err := db.Pool.Exec(ctx, `UPDATE ingestion_jobs SET state='FAILED', attempts=3, reason='OCR_FAILED',
 last_failure_reason='OCR_FAILED', finished_at=clock_timestamp() WHERE job_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := docs.UpdateStatus(ctx, docID, domain.StatusFailed, domain.ReasonOCRFailed); err != nil {
		t.Fatal(err)
	}
	planned, err := q.JobsForDocuments(ctx, []string{docID})
	if err != nil || len(planned[docID]) != 1 {
		t.Fatalf("jobs for documents: %v %v", planned, err)
	}

	// A plan made against an older generation is refused without mutation.
	stale := []DoctorJob{{ID: id, State: "FAILED", Generation: planned[docID][0].Generation - 1}}
	if changed, err := q.ResetToPENDING(ctx, docID, stale); !errors.Is(err, ErrPlanChanged) || changed {
		t.Fatalf("stale plan = %v %v, want ErrPlanChanged", changed, err)
	}
	if got := docStatus(t, db, docID); got != "FAILED" {
		t.Fatalf("a refused item changed the document to %s", got)
	}
	if r := readJob(t, db, id); r.State != "FAILED" || r.Attempts != 3 {
		t.Fatalf("a refused item changed the job: %+v", r)
	}
	// A plan that no longer lists every unfinished job is refused too.
	if _, err := q.ResetToPENDING(ctx, docID, nil); !errors.Is(err, ErrPlanChanged) {
		t.Fatalf("incomplete plan = %v, want ErrPlanChanged", err)
	}

	// The matching plan commits the document, the job and a fresh cycle together.
	if changed, err := q.ResetToPENDING(ctx, docID, planned[docID]); err != nil || !changed {
		t.Fatalf("reset = %v %v", changed, err)
	}
	if got := docStatus(t, db, docID); got != "PENDING" {
		t.Fatalf("document status = %s", got)
	}
	r := readJob(t, db, id)
	if r.State != "READY" || r.Attempts != 0 || r.LastFailure != "" || r.Reason != "" || r.FinishedSet || r.LeaseSet ||
		r.Generation != planned[docID][0].Generation+1 || r.AvailableInFuture {
		t.Fatalf("reset job = %+v, want a fresh READY cycle at the next generation", r)
	}
	again := claimOne(t, jobs, SubjectPDFs)
	if again.ID != id || again.Attempts != 1 || !again.Charged || again.LastFailure != "" {
		t.Fatalf("claim after reset = %+v", again)
	}
}

func TestQueueRecoveryRefusesALiveLeaseButTakesAnExpiredOne(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	q := NewDoctorQueries(db)
	docID, id := addJob(t, db, docs, jobs, SubjectPDFs)
	claimOne(t, jobs, SubjectPDFs)
	if err := docs.UpdateStatus(ctx, docID, domain.StatusProcessing, ""); err != nil {
		t.Fatal(err)
	}
	planned, err := q.JobsForDocuments(ctx, []string{docID})
	if err != nil || len(planned[docID]) != 1 || !planned[docID][0].Live {
		t.Fatalf("a live lease was not reported live: %+v %v", planned, err)
	}
	if _, err := q.ResetToPENDING(ctx, docID, planned[docID]); !errors.Is(err, ErrJobLive) {
		t.Fatalf("reset of a live job = %v, want ErrJobLive", err)
	}
	if r := readJob(t, db, id); r.State != "RUNNING" || r.Attempts != 1 || !r.LeaseSet {
		t.Fatalf("a refused reset disturbed the live claim: %+v", r)
	}
	if got := docStatus(t, db, docID); got != "PROCESSING" {
		t.Fatalf("a refused reset changed the document to %s", got)
	}

	// Once its lease expires the worker is gone and recovery may proceed.
	expire(t, db, id)
	planned, err = q.JobsForDocuments(ctx, []string{docID})
	if err != nil || planned[docID][0].Live {
		t.Fatalf("an expired lease still reads live: %+v %v", planned, err)
	}
	if changed, err := q.ResetToPENDING(ctx, docID, planned[docID]); err != nil || !changed {
		t.Fatalf("reset of an expired job = %v %v", changed, err)
	}
	if r := readJob(t, db, id); r.State != "READY" || r.Attempts != 0 || r.LeaseSet {
		t.Fatalf("after reset: %+v", r)
	}
}

func TestQueueCompletedJobsStayCompleteThroughRecovery(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	q := NewDoctorQueries(db)
	docID, done := addJob(t, db, docs, jobs, SubjectPDFs)
	if _, err := db.Pool.Exec(ctx, `UPDATE ingestion_jobs SET state='COMPLETED', finished_at=clock_timestamp() WHERE job_id=$1`, done); err != nil {
		t.Fatal(err)
	}
	// A second stage's job for the same document failed.
	if err := jobs.Publish(ctx, SubjectEmbed, &ingestionv1.EmbedTextCommand{
		Metadata: &ingestionv1.DocumentMetadata{DocId: docID, RawSha256: domain.SHA256Hex([]byte(docID)), Traceparent: "synthetic"}}, ""); err != nil {
		t.Fatal(err)
	}
	var failed string
	if err := db.Pool.QueryRow(ctx, "SELECT job_id::text FROM ingestion_jobs WHERE doc_id=$1 AND subject=$2", docID, SubjectEmbed).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE ingestion_jobs SET state='FAILED', attempts=3, reason='EXTRACTION_FAILED' WHERE job_id=$1`, failed); err != nil {
		t.Fatal(err)
	}
	if err := docs.UpdateStatus(ctx, docID, domain.StatusFailed, domain.ReasonExtractionFailed); err != nil {
		t.Fatal(err)
	}
	planned, err := q.JobsForDocuments(ctx, []string{docID})
	if err != nil || len(planned[docID]) != 1 || planned[docID][0].ID != failed {
		t.Fatalf("the plan must list only the failed job: %+v %v", planned, err)
	}
	if _, err := q.ResetToPENDING(ctx, docID, planned[docID]); err != nil {
		t.Fatal(err)
	}
	if r := readJob(t, db, done); r.State != "COMPLETED" {
		t.Fatalf("a completed job was redriven: %+v", r)
	}
	if r := readJob(t, db, failed); r.State != "READY" || r.Attempts != 0 {
		t.Fatalf("the failed job was not reset: %+v", r)
	}
}

func TestQueueJobStatsAndStaleProcessingOnBothLayouts(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	q := NewDoctorQueries(db)
	var ids []string
	for i := 0; i < 6; i++ {
		_, id := addJob(t, db, docs, jobs, SubjectPDFs)
		ids = append(ids, id)
	}
	for _, sql := range []string{
		`UPDATE ingestion_jobs SET state='FAILED', reason='OCR_FAILED', finished_at=now() WHERE job_id='` + ids[0] + `'`,
		`UPDATE ingestion_jobs SET state='FAILED', reason='OCR_FAILED', finished_at=now() WHERE job_id='` + ids[1] + `'`,
		`UPDATE ingestion_jobs SET state='FAILED', reason='MALFORMED_COMMAND', finished_at=now() WHERE job_id='` + ids[2] + `'`,
		`UPDATE ingestion_jobs SET state='READY', attempts=3 WHERE job_id='` + ids[3] + `'`, // busy-exhausted
	} {
		if _, err := db.Pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	live := claimOne(t, jobs, SubjectPDFs)
	_ = live
	// ids[3] has 3 attempts and is READY: the claim above may have taken either
	// READY job; make the remaining one an expired RUNNING claim with a spent budget.
	var running string
	if err := db.Pool.QueryRow(ctx, "SELECT job_id::text FROM ingestion_jobs WHERE state='READY' LIMIT 1").Scan(&running); err == nil {
		if _, err := db.Pool.Exec(ctx, `UPDATE ingestion_jobs SET state='RUNNING', attempts=3, claimed_at=clock_timestamp()-interval '10 minutes',
 lease_until=clock_timestamp()-interval '1 minute' WHERE job_id=$1`, running); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := q.JobStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.FailedByReason["OCR_FAILED"] != 2 || stats.FailedByReason["MALFORMED_COMMAND"] != 1 {
		t.Errorf("failed by reason = %v", stats.FailedByReason)
	}
	if stats.ByState["FAILED"] != 3 || stats.ByState["RUNNING"] < 1 {
		t.Errorf("by state = %v", stats.ByState)
	}
	if stats.ExpiredLeases < 1 {
		t.Errorf("expired leases = %d, want the dead worker's claim", stats.ExpiredLeases)
	}
	if stats.ExhaustedBudgets < 1 {
		t.Errorf("exhausted budgets = %d, want the spent-budget claims", stats.ExhaustedBudgets)
	}

	// Stale PROCESSING ignores a document whose job holds a live lease, on the
	// new layout; the old layout, which has no leases, is queried without them.
	if err := docs.UpdateStatus(ctx, live.DocID, domain.StatusProcessing, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE documents SET updated_at = now() - interval '2 hours' WHERE doc_id = $1`, live.DocID); err != nil {
		t.Fatal(err)
	}
	if n, err := q.StaleProcessingCount(ctx, 30*time.Minute, false); err != nil || n != 1 {
		t.Fatalf("stale processing without lease awareness = %d %v, want 1", n, err)
	}
	if n, err := q.StaleProcessingCount(ctx, 30*time.Minute, true); err != nil || n != 0 {
		t.Fatalf("a document with a live lease counted stale: %d %v", n, err)
	}
	downgradeToBrokerLayout(t, db)
	if n, err := q.StaleProcessingCount(ctx, 30*time.Minute, false); err != nil || n != 1 {
		t.Fatalf("old layout stale processing = %d %v", n, err)
	}
	if _, err := q.JobStats(ctx); err == nil {
		t.Fatal("job statistics read an old layout; callers must gate on QueueSchemaReady")
	}
}

func TestQueueScopeAssertionRefusesAnotherWorkspacesDatabase(t *testing.T) {
	db, _ := scratch(t, "queue_scope")
	ctx := context.Background()
	// An empty database is not populated, so any workspace may claim it.
	if err := db.AssertWorkspaceScope(ctx, "anyone"); err != nil {
		t.Fatalf("an empty database refused a workspace: %v", err)
	}
	applySchemaSerialized(t, db, stageBMeta())
	if err := db.AssertWorkspaceScope(ctx, stageBMeta().WorkspaceID); err != nil {
		t.Fatalf("its own workspace was refused: %v", err)
	}
	if err := db.AssertWorkspaceScope(ctx, "another-workspace"); !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("a populated database accepted another workspace: %v", err)
	}
}

// A handler that finished while the run was being torn down still has its
// ownership decided by the fence, not by which context expired first.
func TestQueueFinalizationIsNotCancelledWithTheHandler(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	docID, id := addJob(t, db, docs, jobs, SubjectPDFs)
	j := claimOne(t, jobs, SubjectPDFs)
	runCtx, cancel := context.WithCancel(ctx)
	err := jobs.Run(runCtx, j, func(ctx context.Context) error {
		if err := docs.SaveText(ctx, docID, "finished work", "pdf"); err != nil {
			return err
		}
		cancel() // the run is torn down just as the handler returns
		return nil
	})
	if err != nil {
		t.Fatalf("a finished handler lost its work to a cancelled context: %v", err)
	}
	if r := readJob(t, db, id); r.State != "COMPLETED" {
		t.Fatalf("job = %+v", r)
	}
	var text *string
	if err := db.Pool.QueryRow(ctx, "SELECT normalized_text FROM documents WHERE doc_id=$1", docID).Scan(&text); err != nil || text == nil {
		t.Fatalf("committed work missing: %v %v", text, err)
	}
}

// Recovery selects jobs explicitly. A document's queued and source-waiting jobs
// keep their recorded failures, spent budget and backoff; only its failed and
// expired jobs start a fresh cycle.
func TestQueueRecoveryResetsOnlyFailedAndExpiredJobs(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	q := NewDoctorQueries(db)
	docID, failed := addJob(t, db, docs, jobs, SubjectPDFs)
	meta := &ingestionv1.DocumentMetadata{DocId: docID, RawSha256: domain.SHA256Hex([]byte(docID)), Traceparent: "synthetic"}
	if err := jobs.Publish(ctx, SubjectEmbed, &ingestionv1.EmbedTextCommand{Metadata: meta}, ""); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Publish(ctx, SubjectDocx, &ingestionv1.ProcessOfficeCommand{Metadata: meta, SourceRef: source.Ref(docID)}, ""); err != nil {
		t.Fatal(err)
	}
	var waiting, delayed string
	for subject, dst := range map[string]*string{SubjectEmbed: &waiting, SubjectDocx: &delayed} {
		if err := db.Pool.QueryRow(ctx, "SELECT job_id::text FROM ingestion_jobs WHERE doc_id=$1 AND subject=$2", docID, subject).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	for _, sql := range []string{
		`UPDATE ingestion_jobs SET state='FAILED', attempts=3, reason='OCR_FAILED', last_failure_reason='OCR_FAILED', finished_at=now() WHERE job_id='` + failed + `'`,
		`UPDATE ingestion_jobs SET state='WAITING_SOURCE', attempts=1, reason='SOURCE_CHANGED', last_failure_reason='EXTRACTION_FAILED' WHERE job_id='` + waiting + `'`,
		`UPDATE ingestion_jobs SET attempts=2, last_failure_reason='EMPTY_EXTRACTION', available_at = clock_timestamp() + interval '1 hour' WHERE job_id='` + delayed + `'`,
	} {
		if _, err := db.Pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if err := docs.UpdateStatus(ctx, docID, domain.StatusFailed, domain.ReasonOCRFailed); err != nil {
		t.Fatal(err)
	}
	waitingBefore, delayedBefore := readJob(t, db, waiting), readJob(t, db, delayed)
	planned, err := q.JobsForDocuments(ctx, []string{docID})
	if err != nil || len(planned[docID]) != 3 {
		t.Fatalf("the plan must list every unfinished job: %+v %v", planned, err)
	}
	if changed, err := q.ResetToPENDING(ctx, docID, planned[docID]); err != nil || !changed {
		t.Fatalf("reset = %v %v", changed, err)
	}
	if r := readJob(t, db, failed); r.State != "READY" || r.Attempts != 0 || r.LastFailure != "" {
		t.Errorf("the failed job was not given a fresh cycle: %+v", r)
	}
	if r := readJob(t, db, waiting); r != waitingBefore {
		t.Errorf("a source wait was reset: %+v -> %+v", waitingBefore, r)
	}
	if r := readJob(t, db, delayed); r != delayedBefore || !r.AvailableInFuture || r.Attempts != 2 {
		t.Errorf("a delayed retry lost its backoff or budget: %+v -> %+v", delayedBefore, r)
	}
}

func TestQueueRecoveryLeavesAQueuedDocumentAlone(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	q := NewDoctorQueries(db)
	docID, id := addJob(t, db, docs, jobs, SubjectPDFs) // READY, nothing to recover
	before := readJob(t, db, id)
	planned, err := q.JobsForDocuments(ctx, []string{docID})
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := q.ResetToPENDING(ctx, docID, planned[docID]); err != nil || changed {
		t.Fatalf("reset of a queued document = %v %v, want no change", changed, err)
	}
	if after := readJob(t, db, id); after != before {
		t.Fatalf("a queued job was disturbed: %+v -> %+v", before, after)
	}
}

// An expired handler can still be running, holding its job's advisory lock and
// its document row. Recovery must refuse promptly rather than wait behind it or
// deadlock with its finalization.
func TestQueueRecoveryDoesNotWaitBehindAnExpiredHandler(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	q := NewDoctorQueries(db)
	docID, id := addJob(t, db, docs, jobs, SubjectPDFs)
	j := claimOne(t, jobs, SubjectPDFs)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- jobs.Run(ctx, j, func(ctx context.Context) error {
			// The handler's own status write holds the document row.
			if err := docs.UpdateStatus(ctx, docID, domain.StatusProcessing, ""); err != nil {
				return err
			}
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	expire(t, db, id)
	planned, err := q.JobsForDocuments(ctx, []string{docID})
	if err != nil || len(planned[docID]) != 1 || planned[docID][0].Live {
		t.Fatalf("plan = %+v %v, want the expired job listed as not live", planned, err)
	}
	start := time.Now()
	changed, err := q.ResetToPENDING(ctx, docID, planned[docID])
	if !errors.Is(err, ErrJobLive) || changed {
		t.Fatalf("recovery behind a running handler = %v %v, want ErrJobLive", changed, err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("recovery waited %v behind a running handler", elapsed)
	}
	close(release)
	if err := <-done; !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("the expired handler's completion = %v, want ErrLeaseLost", err)
	}
}

// A failure verdict, exhausted claims included, must not touch a document while
// an expired handler of the same job still holds its advisory lock.
func TestQueueFailRefusesWhileAnExpiredHandlerStillRuns(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	docID, id := addJob(t, db, docs, jobs, SubjectPDFs)
	stale := claimOne(t, jobs, SubjectPDFs)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- jobs.Run(ctx, stale, func(ctx context.Context) error {
			if err := docs.UpdateStatus(ctx, docID, domain.StatusProcessing, ""); err != nil {
				return err
			}
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	// The budget is spent and the lease expired: the next claim is exhausted.
	if _, err := db.Pool.Exec(ctx, `UPDATE ingestion_jobs SET attempts = 3 WHERE job_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	expire(t, db, id)
	successor := claimOne(t, NewJobs(db), SubjectPDFs)
	if successor.Charged {
		t.Fatalf("successor = %+v, want an exhausted claim", successor)
	}
	start := time.Now()
	if err := jobs.Fail(ctx, successor, domain.ReasonUnclassified); !errors.Is(err, ErrJobBusy) {
		t.Fatalf("failing under a running handler = %v, want ErrJobBusy", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the failure verdict waited %v behind the handler", elapsed)
	}
	if r := readJob(t, db, id); r.State != "RUNNING" || r.Attempts != 3 {
		t.Fatalf("a refused failure changed the job: %+v", r)
	}
	// The runtime's answer to that refusal: wait, keeping the budget.
	if err := jobs.Release(ctx, successor, BusyDelay); err != nil {
		t.Fatal(err)
	}
	if r := readJob(t, db, id); r.State != "READY" || r.Attempts != 3 || !r.AvailableInFuture {
		t.Fatalf("busy return of an exhausted claim = %+v, want READY later with the budget retained", r)
	}
	close(release)
	<-done
}

// Alternation is per subject. With claims from different pools interleaved, a
// shared counter would hand one subject the same parity every time and starve
// its expired work.
func TestQueueAlternationIsPerSubject(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	subjects := []string{SubjectPDFs, SubjectEmbed}
	expired := map[string]string{}
	for _, subject := range subjects {
		_, id := addJob(t, db, docs, jobs, subject)
		claimOne(t, jobs, subject)
		expire(t, db, id)
		expired[subject] = id
		for i := 0; i < 4; i++ {
			addJob(t, db, docs, jobs, subject)
		}
	}
	got := map[string][]string{}
	// Interleaved single-slot claims, as concurrent pools issue them.
	for round := 0; round < 2; round++ {
		for _, subject := range subjects {
			got[subject] = append(got[subject], claimOne(t, jobs, subject).ID)
		}
	}
	for _, subject := range subjects {
		found := false
		for _, id := range got[subject] {
			found = found || id == expired[subject]
		}
		if !found {
			t.Errorf("subject %s: two interleaved claims never reached its expired job (%v)", subject, got[subject])
		}
	}
}

// A handler's own status writes stay in its open transaction, so a document
// being processed often still reads PENDING. A live lease means it is working.
func TestQueueLiveWorkIsNotStalePending(t *testing.T) {
	db, docs, jobs := queueSetup(t)
	ctx := context.Background()
	q := NewDoctorQueries(db)
	docID, id := addJob(t, db, docs, jobs, SubjectPDFs)
	if _, err := db.Pool.Exec(ctx, `UPDATE documents SET updated_at = now() - interval '2 hours' WHERE doc_id = $1`, docID); err != nil {
		t.Fatal(err)
	}
	claimOne(t, jobs, SubjectPDFs) // live lease
	if n, err := q.StalePendingCount(ctx, 30*time.Minute, false); err != nil || n != 1 {
		t.Fatalf("without lease awareness = %d %v, want 1", n, err)
	}
	if n, err := q.StalePendingCount(ctx, 30*time.Minute, true); err != nil || n != 0 {
		t.Fatalf("a document under a live lease counted stale: %d %v", n, err)
	}
	expire(t, db, id)
	if n, err := q.StalePendingCount(ctx, 30*time.Minute, true); err != nil || n != 1 {
		t.Fatalf("once the lease expired the document is stale: %d %v", n, err)
	}
}

// The cap on a recovery selection must apply to retryable failures, not to all
// failures: a workspace with many terminal failures older than its retryable
// ones used to hide every retryable document beyond the cap.
func TestQueueRetryableSelectionIsFilteredBeforeItIsCapped(t *testing.T) {
	db, docs, _ := queueSetup(t)
	ctx := context.Background()
	q := NewDoctorQueries(db)
	fail := func(reason domain.FailureReason, age string) string {
		id := domain.NewDocID()
		if _, _, err := docs.CreateStub(ctx, &domain.Document{DocID: id, RawSHA256: domain.SHA256Hex([]byte(id)), DocType: "pdf"}); err != nil {
			t.Fatal(err)
		}
		if err := docs.UpdateStatus(ctx, id, domain.StatusFailed, reason); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, "UPDATE documents SET updated_at = now() - $2::interval WHERE doc_id = $1", id, age); err != nil {
			t.Fatal(err)
		}
		return id
	}
	// Many older terminal and unclassified failures, then newer retryable ones.
	for i := 0; i < 30; i++ {
		fail(domain.ReasonUnknownEncoding, "10 days")
		fail(domain.ReasonUnclassified, "9 days")
	}
	want := map[string]domain.FailureReason{
		fail(domain.ReasonOCRFailed, "2 days"):        domain.ReasonOCRFailed,
		fail(domain.ReasonPDFOpenFailed, "1 day"):     domain.ReasonPDFOpenFailed,
		fail(domain.ReasonEmailParseFailed, "1 hour"): domain.ReasonEmailParseFailed,
	}
	got, err := q.RetryableFAILED(ctx, 10) // far fewer than the 60 older failures
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("selected %d documents, want the %d retryable ones: terminal failures filled the cap", len(got), len(want))
	}
	for _, d := range got {
		if want[d.DocID] != domain.FailureReason(d.Reason) {
			t.Errorf("selected %s with reason %q", d.DocID, d.Reason)
		}
	}
	// The cap still bounds a selection, oldest first.
	capped, err := q.RetryableFAILED(ctx, 2)
	if err != nil || len(capped) != 2 || capped[0].Reason != string(domain.ReasonOCRFailed) {
		t.Fatalf("capped selection = %+v %v, want the two oldest retryable documents", capped, err)
	}
}
