//go:build manual

package postgres

import (
	"context"
	"errors"
	ingestionv1 "github.com/suankan/pocket-advisor/api/proto/v1/gen"
	"github.com/suankan/pocket-advisor/internal/bus"
	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/source"
	"google.golang.org/protobuf/proto"
	"os"
	"testing"
	"time"
)

type recordingPublisher struct {
	jobs []Job
	fail bool
}

func (p *recordingPublisher) PublishJob(_ context.Context, subject string, payload []byte, tp, id string, generation int64) error {
	p.jobs = append(p.jobs, Job{ID: id, Subject: subject, Payload: payload, Generation: generation})
	if p.fail {
		return errors.New("lost publication acknowledgement")
	}
	return nil
}
func setupSourceJob(t *testing.T) (*DB, *DocumentRepo, *Jobs, string, *ingestionv1.ProcessPdfCommand) {
	t.Helper()
	db, _ := scratch(t, "source_"+t.Name())
	ctx := context.Background()
	if err := db.ApplySchema(ctx, stageBMeta()); err != nil {
		t.Fatal(err)
	}
	docs := NewDocumentRepo(db)
	id := domain.NewDocID()
	hash := domain.SHA256Hex([]byte("synthetic"))
	if _, _, err := docs.CreateStub(ctx, &domain.Document{DocID: id, RawSHA256: hash, DocType: "pdf"}); err != nil {
		t.Fatal(err)
	}
	cmd := &ingestionv1.ProcessPdfCommand{Metadata: &ingestionv1.DocumentMetadata{DocId: id, RawSha256: hash, Traceparent: "synthetic"}, SourceRef: source.Ref(id)}
	return db, docs, NewJobs(db), id, cmd
}
func TestSourceJobAtomicHandoffAndDuplicate(t *testing.T) {
	db, docs, jobs, id, cmd := setupSourceJob(t)
	ctx := context.Background()
	crash := errors.New("injected interruption")
	if err := db.Transaction(ctx, func(ctx context.Context) error {
		if err := jobs.Publish(ctx, bus.SubjectPDFs, cmd, ""); err != nil {
			return err
		}
		return crash
	}); !errors.Is(err, crash) {
		t.Fatal(err)
	}
	ready, _, _, _, err := jobs.Counts(ctx)
	if err != nil || ready != 0 {
		t.Fatalf("uncommitted job escaped %d %v", ready, err)
	}
	if err := jobs.Publish(ctx, bus.SubjectPDFs, cmd, ""); err != nil {
		t.Fatal(err)
	}
	publisher := &recordingPublisher{fail: true}
	if _, err := jobs.Dispatch(ctx, publisher, 10); err == nil {
		t.Fatal("publish failure hidden")
	}
	publisher.fail = false
	if _, err := jobs.Dispatch(ctx, publisher, 10); err != nil {
		t.Fatal(err)
	}
	j := publisher.jobs[0]
	if publisher.jobs[1].ID != j.ID {
		t.Fatal("retry changed identity")
	}
	handler := func(ctx context.Context) error {
		if err := docs.SaveText(ctx, id, "canonical text", "pdf", ""); err != nil {
			return err
		}
		return jobs.Publish(ctx, bus.SubjectEmbed, &ingestionv1.EmbedTextCommand{Metadata: cmd.Metadata}, "")
	}
	if err := jobs.Run(ctx, j.ID, j.Subject, j.Payload, func(ctx context.Context) error {
		if err := handler(ctx); err != nil {
			return err
		}
		return crash
	}); !errors.Is(err, crash) {
		t.Fatal(err)
	}
	var text *string
	if err := db.Pool.QueryRow(ctx, "SELECT normalized_text FROM documents WHERE doc_id=$1", id).Scan(&text); err != nil || text != nil {
		t.Fatalf("stage rollback leaked text %v %v", text, err)
	}
	if err := jobs.Run(ctx, j.ID, j.Subject, j.Payload, handler); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Run(ctx, j.ID, j.Subject, j.Payload, func(context.Context) error { t.Fatal("completed handler repeated"); return nil }); err != nil {
		t.Fatal(err)
	}
	ready, _, _, _, err = jobs.Counts(ctx)
	if err != nil || ready != 1 {
		t.Fatalf("downstream job missing %d %v", ready, err)
	}
	if _, err := docs.DeleteDocuments(ctx, []string{id}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Run(ctx, j.ID, j.Subject, j.Payload, func(context.Context) error { t.Fatal("deleted document resurrected"); return nil }); err != nil {
		t.Fatal(err)
	}
}
func TestSourceJobConcurrentDuplicateAndMaintenance(t *testing.T) {
	db, _, jobs, _, cmd := setupSourceJob(t)
	ctx := context.Background()
	if err := jobs.Publish(ctx, bus.SubjectPDFs, cmd, ""); err != nil {
		t.Fatal(err)
	}
	p := &recordingPublisher{}
	_, _ = jobs.Dispatch(ctx, p, 1)
	j := p.jobs[0]
	// A distinct repository has its own pool gate; database locking is the boundary.
	other := NewJobs(db)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- jobs.Run(ctx, j.ID, j.Subject, j.Payload, func(context.Context) error { close(entered); <-release; return nil })
	}()
	<-entered
	if err := other.Run(ctx, j.ID, j.Subject, j.Payload, func(context.Context) error { t.Error("concurrent duplicate ran"); return nil }); !errors.Is(err, ErrJobBusy) {
		t.Fatalf("duplicate=%v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	unlock, err := db.LockMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.LockMaintenance(ctx); !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("maintenance=%v", err)
	}
	unlock()
}
func TestSourceInventoryMigrationAndLocations(t *testing.T) {
	db, docs, _, id, _ := setupSourceJob(t)
	ctx := context.Background()
	inventory := NewSourceRepo(db)
	scan, err := inventory.BeginScan(ctx, []string{"opaque-binding"})
	if err != nil {
		t.Fatal(err)
	}
	if err := inventory.Observe(ctx, scan, id, source.File{RootKey: "opaque-binding", RelativePath: "file.pdf", DisplayPath: "file.pdf", ModifiedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	child := stageDoc(t, docs, "child", "text", "")
	if err := inventory.RecordMember(ctx, id, child, "zip-v1:1"); err != nil {
		t.Fatal(err)
	}
	if err := inventory.FinishScan(ctx, scan); err != nil {
		t.Fatal(err)
	}
	locations, err := inventory.Locations(ctx, []string{child})
	if err != nil || len(locations[child]) != 1 || locations[child][0].Selectors[0] != "zip-v1:1" {
		t.Fatalf("locations=%v %v", locations, err)
	}
	if _, err := db.Pool.Exec(ctx, "ALTER TABLE documents RENAME COLUMN source_ref TO rustfs_raw_uri"); err != nil {
		t.Fatal(err)
	}
	if err := db.ApplySourceSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.ApplySourceSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, err := docs.Exists(ctx, id); err != nil || !ok {
		t.Fatalf("identity lost %v", err)
	}
	incomplete, err := inventory.BeginScan(ctx, []string{"opaque-binding"})
	if err != nil {
		t.Fatal(err)
	}
	locations, _ = inventory.Locations(ctx, []string{id})
	if locations[id][0].Status != "last_observed" {
		t.Fatal("partial scan proved absence")
	}
	if err := inventory.FinishScan(ctx, incomplete); err != nil {
		t.Fatal(err)
	}
	locations, _ = inventory.Locations(ctx, []string{id})
	if locations[id][0].Status != "missing" {
		t.Fatal("complete scan missed absence")
	}
}
func TestSourceJobBrokerLossRedispatch(t *testing.T) {
	url := os.Getenv("SOURCE_TEST_NATS")
	if url == "" {
		t.Skip("set SOURCE_TEST_NATS to disposable NATS")
	}
	_, _, jobs, _, cmd := setupSourceJob(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	b, err := bus.Connect(ctx, url, "source-test-"+domain.NewDocID())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.EnsureStreams(ctx); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Publish(ctx, bus.SubjectPDFs, cmd, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.Dispatch(ctx, b, 1); err != nil {
		t.Fatal(err)
	}
	if err := b.PurgeQueues(ctx); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Reconcile(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.Dispatch(ctx, b, 1); err != nil {
		t.Fatal(err)
	}
	c, err := b.PullConsumer(ctx, bus.StreamName, "source-test", bus.SubjectPDFs)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := c.Fetch(1)
	if err != nil {
		t.Fatal(err)
	}
	for m := range messages.Messages() {
		if m.Headers().Get(GenerationHeader) != "2" {
			t.Fatal("redispatch generation missing")
		}
		var got ingestionv1.ProcessPdfCommand
		if err := proto.Unmarshal(m.Data(), &got); err != nil || got.SourceRef != cmd.SourceRef {
			t.Fatalf("bad source command %v", err)
		}
		_ = m.Ack()
		return
	}
	t.Fatal("broker loss lost durable job")
}

func TestSourceWaitAndGenerationFence(t *testing.T) {
	_, _, jobs, _, cmd := setupSourceJob(t)
	ctx := context.Background()
	if err := jobs.Publish(ctx, bus.SubjectPDFs, cmd, ""); err != nil {
		t.Fatal(err)
	}
	p := &recordingPublisher{}
	if _, err := jobs.Dispatch(ctx, p, 1); err != nil {
		t.Fatal(err)
	}
	j := p.jobs[0]
	if err := jobs.Waiting(ctx, j.ID, "SOURCE_CHANGED"); err != nil {
		t.Fatal(err)
	}
	if err := jobs.CheckResolved(ctx); err == nil {
		t.Fatal("missing source counted as success")
	}
	if err := jobs.ResumeSources(ctx); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Run(ctx, j.ID, j.Subject, j.Payload, func(context.Context) error { t.Fatal("stale generation ran"); return nil }, 1); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Run(ctx, j.ID, j.Subject, j.Payload, func(context.Context) error { return nil }, 2); err != nil {
		t.Fatal(err)
	}
	if err := jobs.CheckResolved(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSourceMigrationPendingDeletionGate(t *testing.T) {
	db, _, _, _, _ := setupSourceJob(t)
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, `ALTER TABLE documents RENAME COLUMN source_ref TO rustfs_raw_uri; CREATE TABLE pending_object_deletions(raw_sha256 text); INSERT INTO pending_object_deletions VALUES('synthetic')`); err != nil {
		t.Fatal(err)
	}
	if err := db.ApplySourceSchema(ctx); err == nil {
		t.Fatal("unresolved legacy cleanup allowed migration")
	}
	var old bool
	if err := db.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='documents' AND column_name='rustfs_raw_uri')`).Scan(&old); err != nil || !old {
		t.Fatalf("migration partially committed %v", err)
	}
	if _, err := db.Pool.Exec(ctx, `DELETE FROM pending_object_deletions`); err != nil {
		t.Fatal(err)
	}
	if err := db.ApplySourceSchema(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSourceWipeRemovesActiveGraph(t *testing.T) {
	db, docs, _, _, _ := setupSourceJob(t)
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO topic_graph_versions(version_id,status,extraction_version,config_version,max_mentions_per_doc,max_spans_per_mention,max_display_label_bytes) VALUES(gen_random_uuid(),'BUILDING','synthetic','synthetic',1,1,20);
INSERT INTO topic_mentions(mention_id,version_id,doc_id,extraction_version) SELECT gen_random_uuid(),v.version_id,d.doc_id,'synthetic' FROM topic_graph_versions v CROSS JOIN documents d;
UPDATE topic_graph_versions SET status='READY'; UPDATE topic_graph_versions SET status='ACTIVE'`); err != nil {
		t.Fatal(err)
	}
	if _, err := docs.DeleteWorkspace(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM topic_graph_versions`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("retained graph: %d %v", n, err)
	}
}
