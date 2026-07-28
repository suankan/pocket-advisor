// Command email-processor unrolls MIME structures and archive containers
// (ingestion-design.md §5.3).
package main

import (
	"fmt"
	"os"

	"github.com/suankan/pocket-advisor/v3/internal/app"
	"github.com/suankan/pocket-advisor/v3/internal/bus"
	"github.com/suankan/pocket-advisor/v3/internal/worker"
)

func main() {
	a, err := app.New("email-processor", app.Needs{MinIO: true, Postgres: true, NATS: true})
	if err != nil {
		fmt.Fprintf(os.Stderr, "email-processor: %v\n", err)
		os.Exit(1)
	}
	defer a.Close()

	consumer, err := a.Bus.PullConsumer(a.Ctx, "email-processor", bus.SubjectEmails)
	if err != nil {
		a.Log.Error("consumer", "error", err)
		os.Exit(1)
	}

	w := &worker.EmailWorker{Vault: a.Vault, Docs: a.Docs, Bus: a.Bus, Log: a.Log}
	rt := &worker.Runtime{
		Name: "email-processor", Bus: a.Bus, Docs: a.Docs, Log: a.Log,
		Subject: bus.SubjectEmails,
		// I/O bound: batch fetch.
		Batch: 8,
	}
	if err := rt.Consume(a.Ctx, consumer, w.Handle); err != nil {
		a.Log.Error("consume", "error", err)
		os.Exit(1)
	}
	a.Log.Info("shutdown complete")
}
