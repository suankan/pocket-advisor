//go:build manual

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	ingestionv1 "github.com/suankan/pocket-advisor/api/proto/v1/gen"
	"github.com/suankan/pocket-advisor/internal/client/embedding"
	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/engine/embed"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
)

// An attempt that fails part-way keeps the vectors it got, so the retry sends
// only the passages still missing, and the document is stored whole, in
// order, once every passage has one.
func TestEmbeddingResumesFromTheBatchesItHas(t *testing.T) {
	db, _, _ := admissionFixture(t)
	ctx := context.Background()
	docs := postgres.NewDocumentRepo(db)

	var text strings.Builder
	for i := 0; text.Len() < 30*embed.TargetChars; i++ {
		fmt.Fprintf(&text, "Paragraph %d of a synthetic letter about the schedule and the budget.\n\n", i)
	}
	id, _, err := docs.CreateStub(ctx, &domain.Document{DocID: domain.NewDocID(), RawSHA256: domain.SHA256Hex([]byte(text.String())), DocType: "text"})
	if err != nil {
		t.Fatal(err)
	}
	if err := docs.SaveText(ctx, id, text.String(), "text"); err != nil {
		t.Fatal(err)
	}

	var requests, sent atomic.Int64
	failSecond := atomic.Bool{}
	failSecond.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Input []string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		if requests.Add(1) == 2 && failSecond.Load() {
			http.Error(w, "synthetic overload", http.StatusServiceUnavailable)
			return
		}
		sent.Add(int64(len(req.Input)))
		type datum struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		out := struct {
			Data  []datum        `json:"data"`
			Usage map[string]int `json:"usage"`
		}{Usage: map[string]int{"prompt_tokens": 0}}
		for i, in := range req.Input {
			out.Data = append(out.Data, datum{i, []float32{1, 0, 0, 0, 0, 0, 0, float32(len(in)%7) / 7}})
			out.Usage["prompt_tokens"] += len(in) / 4
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	w := &EmbedWorker{
		Docs: docs, Chunks: postgres.NewChunkRepo(db),
		Embedder: embedding.New(config.Embedding{Endpoint: srv.URL, Model: "synthetic", Timeout: 5 * time.Second, Concurrency: 1}),
		Cache:    NewVectorCache(1 << 20), Log: quietLogger(),
	}
	payload, err := proto.Marshal(&ingestionv1.EmbedTextCommand{Metadata: &ingestionv1.DocumentMetadata{DocId: id, Traceparent: "synthetic"}})
	if err != nil {
		t.Fatal(err)
	}
	job := postgres.Job{ID: "synthetic", DocID: id, Payload: payload}

	if _, err := w.Prepare(ctx, job); err == nil {
		t.Fatal("the attempt whose second request failed succeeded")
	}
	first := sent.Load()
	total := int64(len(embed.Split(text.String())))
	if first == 0 || first >= total {
		t.Fatalf("first attempt embedded %d of %d passages; the fixture needs several batches", first, total)
	}

	failSecond.Store(false)
	commit, err := w.Prepare(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if again := sent.Load() - first; again != total-first {
		t.Errorf("retry sent %d passages, want only the %d still missing", again, total-first)
	}
	if err := db.Transaction(ctx, func(ctx context.Context) error { return commit(ctx, job) }); err != nil {
		t.Fatal(err)
	}
	var stored, ordered int64
	if err := db.Pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE chunk_index = ord - 1)
	 FROM (SELECT chunk_index, row_number() OVER (ORDER BY chunk_index) AS ord FROM document_chunks WHERE doc_id = $1) p`, id).Scan(&stored, &ordered); err != nil {
		t.Fatal(err)
	}
	if stored != total || ordered != total {
		t.Fatalf("stored %d passages, %d in sequence; want %d", stored, ordered, total)
	}
}
