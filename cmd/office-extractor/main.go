// Command office-extractor extracts text from OOXML and related formats.
// Pure Go, no CGo — see ingestion-design.md §5.5 and §8.3.
package main

import (
	"fmt"
	"os"

	"github.com/suankan/pocket-advisor/v3/internal/app"
	"github.com/suankan/pocket-advisor/v3/internal/bus"
	"github.com/suankan/pocket-advisor/v3/internal/worker"
)

func main() {
	a, err := app.New("office-extractor", app.Needs{MinIO: true, Postgres: true, NATS: true})
	if err != nil {
		fmt.Fprintf(os.Stderr, "office-extractor: %v\n", err)
		os.Exit(1)
	}
	defer a.Close()

	consumer, err := a.Bus.PullConsumer(a.Ctx, "office-extractor", bus.SubjectDocx)
	if err != nil {
		a.Log.Error("consumer", "error", err)
		os.Exit(1)
	}

	w := &worker.OfficeWorker{Vault: a.Vault, Docs: a.Docs, Bus: a.Bus, Log: a.Log}
	rt := &worker.Runtime{
		Name: "office-extractor", Bus: a.Bus, Docs: a.Docs, Log: a.Log,
		Subject: bus.SubjectDocx,
		Batch:   4,
	}
	if err := rt.Consume(a.Ctx, consumer, w.Handle); err != nil {
		a.Log.Error("consume", "error", err)
		os.Exit(1)
	}
	a.Log.Info("shutdown complete")
}
