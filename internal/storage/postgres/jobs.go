package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"time"

	"github.com/jackc/pgx/v5"
	ingestionv1 "github.com/suankan/pocket-advisor/api/proto/v1/gen"
	"github.com/suankan/pocket-advisor/internal/domain"
	"google.golang.org/protobuf/proto"
)

var ErrWorkspaceBusy = errors.New("another ingestion or maintenance process owns this workspace")
var ErrJobBusy = errors.New("job is already running")
var ErrInvalidJob = errors.New("invalid or incompatible ingestion job")

const JobHeader = "X-Ingestion-Job"
const GenerationHeader = "X-Ingestion-Generation"

type Jobs struct {
	db    *DB
	slots chan struct{}
}

func NewJobs(db *DB) *Jobs {
	return &Jobs{db: db, slots: make(chan struct{}, max(1, int(db.Pool.Config().MaxConns)-3))}
}
func (r *Jobs) Publish(ctx context.Context, subject string, msg proto.Message, traceparent string) error {
	m, ok := msg.(interface {
		GetMetadata() *ingestionv1.DocumentMetadata
	})
	if !ok || m.GetMetadata() == nil {
		return ErrInvalidJob
	}
	meta := m.GetMetadata()
	payload, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	// Stable stage identity is raw bytes plus canonical extraction for embedding.
	revision := meta.RawSha256
	if _, ok := msg.(*ingestionv1.EmbedTextCommand); ok {
		var hash string
		if err := r.db.executor(ctx).QueryRow(ctx, "SELECT extraction_hash FROM documents WHERE doc_id=$1", meta.DocId).Scan(&hash); err != nil {
			return err
		}
		revision += "/" + hash
	}
	_, err = r.db.executor(ctx).Exec(ctx, `INSERT INTO ingestion_jobs(doc_id,subject,revision,payload,traceparent)
 VALUES($1,$2,$3,$4,$5) ON CONFLICT(doc_id,subject,revision) DO NOTHING`, meta.DocId, subject, revision, payload, traceparent)
	return err
}

type Job struct {
	ID, DocID, Subject, Traceparent string
	Payload                         []byte
	Generation                      int64
}
type JobPublisher interface {
	PublishJob(context.Context, string, []byte, string, string, int64) error
}

func (r *Jobs) Dispatch(ctx context.Context, publisher JobPublisher, limit int) (int, error) {
	var jobs []Job
	err := r.db.Transaction(ctx, func(ctx context.Context) error {
		rows, err := r.db.executor(ctx).Query(ctx, `WITH selected AS (
   SELECT job_id FROM ingestion_jobs WHERE state='READY' AND (dispatch_lease_until IS NULL OR dispatch_lease_until<now())
   ORDER BY created_at LIMIT $1 FOR UPDATE SKIP LOCKED)
   UPDATE ingestion_jobs j SET dispatch_lease_until=now()+interval '30 seconds' FROM selected s WHERE j.job_id=s.job_id
   RETURNING j.job_id::text,j.doc_id::text,j.subject,j.payload,j.traceparent,j.generation`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var j Job
			if err := rows.Scan(&j.ID, &j.DocID, &j.Subject, &j.Payload, &j.Traceparent, &j.Generation); err != nil {
				return err
			}
			jobs = append(jobs, j)
		}
		return rows.Err()
	})
	if err != nil {
		return 0, err
	}
	for i, j := range jobs {
		if err := publisher.PublishJob(ctx, j.Subject, j.Payload, j.Traceparent, j.ID, j.Generation); err != nil {
			_, _ = r.db.Pool.Exec(ctx, "UPDATE ingestion_jobs SET dispatch_lease_until=NULL WHERE job_id=$1 AND state='READY'", j.ID)
			return i, err
		}
		// A fast consumer can finish before this update. Never overwrite its verdict.
		if _, err := r.db.Pool.Exec(ctx, `UPDATE ingestion_jobs SET state='DISPATCHED',dispatched_at=now(),dispatch_lease_until=NULL WHERE job_id=$1 AND generation=$2 AND state='READY'`, j.ID, j.Generation); err != nil {
			return i + 1, err
		}
	}
	return len(jobs), nil
}

// Run serializes deliveries with a transaction lock and commits all stage
// writes, downstream jobs, and completion together. The handler deadline bounds
// the transaction; nested repository transactions use savepoints on this one.
func (r *Jobs) Run(ctx context.Context, id, subject string, payload []byte, fn func(context.Context) error, generation ...int64) error {
	if parsed, err := uuid.Parse(id); err != nil || parsed.String() != id {
		return ErrInvalidJob
	}
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	return r.db.Transaction(ctx, func(ctx context.Context) error {
		var locked bool
		if err := r.db.executor(ctx).QueryRow(ctx, "SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))", id).Scan(&locked); err != nil {
			return err
		}
		if !locked {
			return ErrJobBusy
		}
		var state, docID, storedSubject string
		var stored []byte
		var currentGeneration int64
		err := r.db.executor(ctx).QueryRow(ctx, "SELECT state,doc_id::text,subject,payload,generation FROM ingestion_jobs WHERE job_id=$1 FOR UPDATE", id).Scan(&state, &docID, &storedSubject, &stored, &currentGeneration)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if len(generation) > 0 && generation[0] != currentGeneration {
			if generation[0] > 0 && generation[0] < currentGeneration {
				return nil
			}
			return ErrInvalidJob
		}
		if subject != storedSubject || !bytes.Equal(payload, stored) {
			return ErrInvalidJob
		}
		if state == "COMPLETED" || state == "FAILED" || state == "WAITING_SOURCE" {
			return nil
		}
		if err := fn(ctx); err != nil {
			return err
		}
		_, err = r.db.executor(ctx).Exec(ctx, "UPDATE ingestion_jobs SET state='COMPLETED',finished_at=now(),dispatch_lease_until=NULL WHERE job_id=$1", id)
		return err
	})
}
func (r *Jobs) Waiting(ctx context.Context, id, reason string) error {
	_, err := r.db.Pool.Exec(ctx, "UPDATE ingestion_jobs SET state='WAITING_SOURCE',reason=$2,dispatch_lease_until=NULL WHERE job_id=$1 AND state NOT IN ('COMPLETED','FAILED')", id, reason)
	return err
}
func (r *Jobs) Fail(ctx context.Context, id string, reason domain.FailureReason) error {
	return r.db.Transaction(ctx, func(ctx context.Context) error {
		var docID string
		err := r.db.executor(ctx).QueryRow(ctx, "UPDATE ingestion_jobs SET state='FAILED',reason=$2,finished_at=now(),dispatch_lease_until=NULL WHERE job_id=$1 AND state<>'COMPLETED' RETURNING doc_id::text", id, string(reason)).Scan(&docID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return NewDocumentRepo(r.db).UpdateStatus(ctx, docID, domain.StatusFailed, reason)
	})
}

// Reconcile explicitly reissues unfinished published jobs, including those lost
// from the broker. Completed and terminal jobs are never silently redriven.
func (r *Jobs) Reconcile(ctx context.Context, older time.Duration) error {
	_, err := r.db.Pool.Exec(ctx, `UPDATE ingestion_jobs SET state='READY',generation=generation+1,dispatch_lease_until=NULL
 WHERE state IN ('DISPATCHED','WAITING_SOURCE') AND COALESCE(dispatched_at,created_at)<now()-$1::interval`, older.String())
	return err
}
func (r *Jobs) Counts(ctx context.Context) (ready, unfinished, waiting, failed int, err error) {
	err = r.db.Pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE state='READY'),count(*) FILTER(WHERE state IN ('READY','DISPATCHED')),
 count(*) FILTER(WHERE state='WAITING_SOURCE'),count(*) FILTER(WHERE state='FAILED') FROM ingestion_jobs`).Scan(&ready, &unfinished, &waiting, &failed)
	return
}
func (r *Jobs) CheckResolved(ctx context.Context) error {
	_, u, w, f, err := r.Counts(ctx)
	if err != nil {
		return err
	}
	if u+w+f > 0 {
		return fmt.Errorf("ingestion unresolved: %d unfinished, %d waiting for sources, %d failed jobs", u, w, f)
	}
	return nil
}

// ResumeSources retries source waits after a new complete filesystem scan.
func (r *Jobs) ResumeSources(ctx context.Context) error {
	_, err := r.db.Pool.Exec(ctx, `UPDATE ingestion_jobs SET state='READY',generation=generation+1,dispatch_lease_until=NULL WHERE state='WAITING_SOURCE'`)
	return err
}
