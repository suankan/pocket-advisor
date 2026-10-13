//go:build manual

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/suankan/pocket-advisor/internal/domain"
)

// Database-backed checks of content admission under concurrency. Manual
// because what is under test is PostgreSQL's locking, which no fake models.
//
//	Run: EMAIL_DSN=postgres://<role>@localhost:5432/<disposable-db> \
//		go test -tags manual ./internal/storage/postgres/ -run CreateStub -v

// A stage handler writes its document's row at the start of a transaction it
// holds for the whole extraction. Admitting a second copy of that content, and
// linking it into another container, must not wait for that transaction: a
// shared attachment under OCR would otherwise hold every email that carries it.
func TestCreateStubDoesNotWaitOnARowHeldByItsStageHandler(t *testing.T) {
	db, _ := scratch(t, "admit_held")
	ctx := context.Background()
	if err := db.ApplySchema(ctx, stageBMeta()); err != nil {
		t.Fatal(err)
	}
	r := NewDocumentRepo(db)
	held := stageDoc(t, r, "held-content", "pdf", "")
	container := stageDoc(t, r, "second-email", "email", "")

	handler, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Rollback(ctx)
	if _, err := handler.Exec(ctx, `UPDATE documents SET processing_status='PROCESSING' WHERE doc_id=$1`, held); err != nil {
		t.Fatal(err)
	}

	deadline, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	err = db.Transaction(deadline, func(ctx context.Context) error {
		id, created, err := r.CreateStub(ctx, &domain.Document{DocID: domain.NewDocID(), RawSHA256: "held-content", DocType: "pdf"})
		if err != nil {
			return err
		}
		if id != held || created {
			t.Errorf("CreateStub = (%s, %v), want the held document (%s, false)", id, created, held)
		}
		return r.RecordPlacement(ctx, container, id)
	})
	if err != nil {
		t.Fatalf("admission waited on the stage handler's row: %v", err)
	}
}

// Two admissions of the same new bytes: the second waits for the first's
// transaction and then resolves to whatever it left behind — its document when
// it committed, a fresh one of its own when it rolled back. Never a duplicate.
func TestCreateStubRaceResolvesToTheCommittedWinner(t *testing.T) {
	for _, commit := range []bool{true, false} {
		name := "first rolls back"
		if commit {
			name = "first commits"
		}
		t.Run(name, func(t *testing.T) {
			db, _ := scratch(t, "admit_race")
			ctx := context.Background()
			if err := db.ApplySchema(ctx, stageBMeta()); err != nil {
				t.Fatal(err)
			}
			r := NewDocumentRepo(db)
			const sha = "raced-content"

			rollback := errors.New("first admission rolls back")
			admitted, finish, firstDone := make(chan string, 1), make(chan struct{}), make(chan error, 1)
			go func() {
				firstDone <- db.Transaction(ctx, func(ctx context.Context) error {
					id, created, err := r.CreateStub(ctx, &domain.Document{DocID: domain.NewDocID(), RawSHA256: sha, DocType: "pdf"})
					if err != nil {
						return err
					}
					if !created {
						return errors.New("first admission found existing content")
					}
					admitted <- id
					<-finish
					if !commit {
						return rollback
					}
					return nil
				})
			}()
			var first string
			select {
			case first = <-admitted:
			case err := <-firstDone:
				t.Fatalf("first admission: %v", err)
			}

			type result struct {
				id      string
				created bool
				err     error
			}
			second := make(chan result, 1)
			go func() {
				id, created, err := r.CreateStub(ctx, &domain.Document{DocID: domain.NewDocID(), RawSHA256: sha, DocType: "pdf"})
				second <- result{id, created, err}
			}()
			select {
			case got := <-second:
				t.Fatalf("second admission did not wait for the uncommitted first: %+v", got)
			case <-time.After(300 * time.Millisecond):
			}

			close(finish)
			if err := <-firstDone; err != nil && !errors.Is(err, rollback) {
				t.Fatalf("first admission: %v", err)
			}
			got := <-second
			if got.err != nil {
				t.Fatalf("second admission: %v", got.err)
			}
			if commit && (got.id != first || got.created) {
				t.Errorf("after commit: (%s, %v), want the winner (%s, false)", got.id, got.created, first)
			}
			if !commit && (got.id == first || !got.created) {
				t.Errorf("after rollback: (%s, %v), want a new document of its own", got.id, got.created)
			}
			var rows int
			if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM documents WHERE raw_sha256=$1`, sha).Scan(&rows); err != nil || rows != 1 {
				t.Fatalf("documents holding the content = %d (%v), want 1", rows, err)
			}
		})
	}
}
