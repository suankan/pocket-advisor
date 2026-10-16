package worker

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	ingestionv1 "github.com/suankan/pocket-advisor/api/proto/v1/gen"
	"github.com/suankan/pocket-advisor/internal/client/embedding"
	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/engine/embed"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/telemetry"
	"github.com/suankan/pocket-advisor/internal/trace"
)

type EmbedWorker struct {
	Docs     *postgres.DocumentRepo
	Chunks   *postgres.ChunkRepo
	Embedder *embedding.Client
	// Cache, when set, keeps vectors across a document's attempts.
	Cache *VectorCache
	Log   *slog.Logger
}

// Handle embeds a document's text and commits its passages in one step. The
// pipeline runs the two halves apart, Prepare outside the job's transaction
// and its result inside it.
func (w *EmbedWorker) Handle(ctx context.Context, job postgres.Job) error {
	commit, err := w.Prepare(ctx, job)
	if err != nil {
		return err
	}
	return commit(ctx, job)
}

// Prepare reads, chunks and embeds a document's text, and returns the handler
// that stores the passages and completes the document.
//
// It holds no transaction: embedding a large document takes minutes against
// an endpoint that serves one request at a time, and an open transaction
// would hold a connection and a gate slot for all of it. The text cannot
// change underneath: it is written before this job is published, and a
// reroute or refusal leaves a document with a running job alone.
func (w *EmbedWorker) Prepare(ctx context.Context, job postgres.Job) (Handler, error) {
	var cmd ingestionv1.EmbedTextCommand
	if err := proto.Unmarshal(job.Payload, &cmd); err != nil {
		return nil, Fatal(domain.ReasonMalformedCommand, err)
	}
	meta := cmd.Metadata
	if meta == nil || meta.Traceparent == "" {
		return nil, Fatal(domain.ReasonMissingTraceContext, fmt.Errorf("command carries no traceparent"))
	}

	// The command carries a reference, not the text (§4.1).
	loaded, err := w.Docs.LoadText(ctx, meta.DocId)
	if err != nil {
		return nil, WithDoc(meta.DocId, err)
	}
	if strings.TrimSpace(loaded.Text) == "" {
		return nil, Decline(meta.DocId, domain.ReasonEmptyExtraction)
	}

	// Chunk first, then batch: the token budget must bound what leaves the
	// process, and the chunker is what multiplies token count (§2.4).
	pieces := embed.Split(loaded.Text)
	if len(pieces) == 0 {
		return nil, Decline(meta.DocId, domain.ReasonEmptyExtraction)
	}

	model := w.Embedder.Model()
	chunks := make([]domain.Chunk, 0, len(pieces))
	chunkOf := func(c embed.Chunk, vector []float32) domain.Chunk {
		return domain.Chunk{
			ChunkID:    domain.NewChunkID(),
			DocID:      meta.DocId,
			Index:      c.Index,
			StartChar:  c.Start,
			EndChar:    c.End,
			Text:       c.Text,
			EmbedModel: model,
			Embedding:  vector,
		}
	}

	// Passages an earlier attempt already embedded are not sent again.
	var todo []embed.Chunk
	for _, c := range pieces {
		if v, ok := w.Cache.Get(model, c.Text); ok {
			chunks = append(chunks, chunkOf(c, v))
			continue
		}
		todo = append(todo, c)
	}

	batches := embed.NewBatcher(todo)
	for batch := batches.Next(); batch != nil; batch = batches.Next() {
		// A chunk is embedded as exactly its own text — no subject, no
		// filename, nothing borrowed from the document or thread it belongs
		// to. Sharing a prefix across every chunk of a container pulls them
		// into one neighbourhood and blunts the distinctions a query needs;
		// what a chunk is part of is recovered at retrieval by doc_id, which
		// is an exact lookup rather than a lossy encoding (§5.6).
		inputs := make([]string, len(batch))
		for i, c := range batch {
			inputs[i] = c.Text
		}

		vectors, tokens, err := w.Embedder.EmbedCounted(ctx, inputs)
		if err != nil {
			// Transient: a model restart should cost latency, not documents.
			// The breaker keeps retries from hammering it, and the reason keeps
			// a job whose attempts ran out where --recover finds it.
			return nil, WithDoc(meta.DocId, Retry(domain.ReasonEmbeddingUnavailable, err))
		}
		batches.Observe(batch, tokens)
		if tokens == 0 {
			for _, in := range inputs {
				tokens += len(in) / embed.CharsPerToken
			}
		}
		telemetry.EmbeddingTokens.Add(float64(tokens))

		for i, c := range batch {
			w.Cache.Put(model, c.Text, vectors[i])
			chunks = append(chunks, chunkOf(c, vectors[i]))
		}
	}
	slices.SortFunc(chunks, func(a, b domain.Chunk) int { return a.Index - b.Index })

	return func(ctx context.Context, _ postgres.Job) error {
		// One transaction for every chunk plus the status update: a document
		// whose chunk count exceeds the batch budget is split across several
		// embedding requests but is never half-indexed (§2.4).
		if err := w.Chunks.ReplaceChunks(ctx, meta.DocId, chunks); err != nil {
			return WithDoc(meta.DocId, err)
		}
		w.Log.Info("indexed",
			"doc_id", meta.DocId, "chunks", len(chunks), "model", model,
			"trace_id", trace.TraceID(meta.Traceparent))
		return nil
	}, nil
}
