// Command embed-indexer chunks, embeds and persists (ingestion-design.md §5.6).
package main

import (
	"fmt"
	"os"

	"github.com/suankan/pocket-advisor/internal/app"
	"github.com/suankan/pocket-advisor/internal/bus"
	"github.com/suankan/pocket-advisor/internal/client/embedding"
	"github.com/suankan/pocket-advisor/internal/worker"
)

func main() {
	a, err := app.New("embed-indexer", app.Needs{Postgres: true, NATS: true})
	if err != nil {
		fmt.Fprintf(os.Stderr, "embed-indexer: %v\n", err)
		os.Exit(1)
	}
	defer a.Close()

	if a.Cfg.Embedding.Endpoint == "" {
		a.Log.Error("EMBEDDING_ENDPOINT is required")
		os.Exit(1)
	}
	client := embedding.New(a.Cfg.Embedding)

	// Fatal startup check, not a warning: a worker that embeds at one
	// dimension into a column sized for another writes vectors that are
	// silently not comparable to their neighbours (§4.4).
	info, err := client.Probe(a.Ctx)
	if err != nil {
		a.Log.Error("embedding endpoint probe failed", "error", err)
		os.Exit(1)
	}
	if err := a.DB.VerifyDimension(a.Ctx, a.Cfg.Embedding.Model, info.Dimension); err != nil {
		a.Log.Error("dimension check failed", "error", err,
			"endpoint_dim", info.Dimension, "model", a.Cfg.Embedding.Model)
		os.Exit(1)
	}
	a.Log.Info("embedding endpoint verified",
		"model", a.Cfg.Embedding.Model, "dimension", info.Dimension)

	consumer, err := a.Bus.PullConsumer(a.Ctx, "embed-indexer", bus.SubjectEmbed)
	if err != nil {
		a.Log.Error("consumer", "error", err)
		os.Exit(1)
	}

	w := &worker.EmbedWorker{Docs: a.Docs, Chunks: a.Chunks, Embedder: client, Log: a.Log}
	rt := &worker.Runtime{
		Name: "embed-indexer", Bus: a.Bus, Docs: a.Docs, Log: a.Log,
		Subject: bus.SubjectEmbed,
		Batch:   4,
	}
	if err := rt.Consume(a.Ctx, consumer, w.Handle); err != nil {
		a.Log.Error("consume", "error", err)
		os.Exit(1)
	}
	a.Log.Info("shutdown complete")
}
