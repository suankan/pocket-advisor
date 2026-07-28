package worker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nats-io/nats.go/jetstream"
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
	Log      *slog.Logger
}

func (w *EmbedWorker) Handle(ctx context.Context, msg jetstream.Msg) error {
	var cmd ingestionv1.EmbedTextCommand
	if err := proto.Unmarshal(msg.Data(), &cmd); err != nil {
		return Fatal("MALFORMED_COMMAND", err)
	}
	meta := cmd.Metadata
	if meta == nil || meta.Traceparent == "" {
		return Fatal("MISSING_TRACE_CONTEXT", fmt.Errorf("command carries no traceparent"))
	}

	// The command carries a reference, not the text (§4.1).
	text, workspace, err := w.Docs.LoadText(ctx, meta.DocId)
	if err != nil {
		return WithDoc(meta.DocId, err)
	}
	if strings.TrimSpace(text) == "" {
		return Decline(meta.DocId, domain.ReasonEmptyExtraction)
	}
	if workspace == "" {
		workspace = meta.WorkspaceId
	}

	// Chunk first, then batch: the token budget must bound what leaves the
	// process, and the chunker is what multiplies token count (§2.4).
	pieces := embed.Split(text)
	if len(pieces) == 0 {
		return Decline(meta.DocId, domain.ReasonEmptyExtraction)
	}

	model := w.Embedder.Model()
	chunks := make([]domain.Chunk, 0, len(pieces))

	for _, batch := range embed.Batches(pieces) {
		inputs := make([]string, len(batch))
		for i, c := range batch {
			inputs[i] = c.Text
			telemetry.EmbeddingTokens.Add(float64(len(c.Text) / embed.CharsPerToken))
		}

		vectors, err := w.Embedder.Embed(ctx, inputs)
		if err != nil {
			// Transient by default: a model restart should cost latency, not
			// documents. The breaker keeps redeliveries from hammering it.
			return WithDoc(meta.DocId, err)
		}

		for i, c := range batch {
			chunks = append(chunks, domain.Chunk{
				ChunkID:    domain.NewChunkID(meta.DocId, model, c.Index),
				DocID:      meta.DocId,
				Workspace:  workspace,
				Index:      c.Index,
				StartChar:  c.Start,
				EndChar:    c.End,
				Text:       c.Text,
				EmbedModel: model,
				Embedding:  vectors[i],
			})
		}
	}

	// One transaction for every chunk plus the status update: a document whose
	// chunk count exceeds the batch budget is split across several embedding
	// requests but is never half-indexed (§2.4).
	if err := w.Chunks.ReplaceChunks(ctx, meta.DocId, chunks); err != nil {
		return WithDoc(meta.DocId, err)
	}

	w.Log.Info("indexed",
		"doc_id", meta.DocId, "chunks", len(chunks), "model", model,
		"trace_id", trace.TraceID(meta.Traceparent))
	return nil
}
