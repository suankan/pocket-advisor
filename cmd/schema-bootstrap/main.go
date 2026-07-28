// Command schema-bootstrap resolves the vector dimension from the embedding
// endpoint and applies the DDL (ingestion-design.md §4.4).
//
// halfvec(N) is a typed SQL column, so N must be known before the first
// CREATE TABLE — but the authority on N is the model, not a design document.
// Pinning a literal in checked-in DDL is how an index silently ends up the
// wrong shape when the endpoint changes.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/suankan/pocket-advisor/internal/app"
	"github.com/suankan/pocket-advisor/internal/client/embedding"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
)

func main() {
	var (
		truncated = flag.Bool("matryoshka-truncated", false,
			"the endpoint returns a deliberately truncated dimension")
		override = flag.Int("dimension", 0,
			"skip the probe and use this dimension (escape hatch only)")
	)
	flag.Parse()

	a, err := app.New("schema-bootstrap", app.Needs{Postgres: true})
	if err != nil {
		fmt.Fprintf(os.Stderr, "schema-bootstrap: %v\n", err)
		os.Exit(1)
	}
	defer a.Close()

	dim := *override
	model := a.Cfg.Embedding.Model

	if dim == 0 {
		if a.Cfg.Embedding.Endpoint == "" {
			fmt.Fprintln(os.Stderr,
				"schema-bootstrap: EMBEDDING_ENDPOINT is required (or pass --dimension)")
			os.Exit(1)
		}
		client := embedding.New(a.Cfg.Embedding)
		info, err := client.Probe(a.Ctx)
		if err != nil {
			a.Log.Error("probe failed", "endpoint", a.Cfg.Embedding.Endpoint, "error", err)
			os.Exit(1)
		}
		dim = info.Dimension
		if info.Model != "" {
			model = info.Model
		}
		a.Log.Info("probed embedding endpoint", "model", model, "dimension", dim)
	} else {
		a.Log.Warn("dimension override in use; the endpoint was not consulted", "dimension", dim)
	}

	meta := postgres.SchemaMetadata{
		EmbedModel:   model,
		EmbedDim:     dim,
		TruncatedDim: *truncated,
	}
	if err := a.DB.ApplySchema(a.Ctx, meta); err != nil {
		a.Log.Error("apply schema failed", "error", err)
		os.Exit(1)
	}

	a.Log.Info("schema ready", "model", model, "dimension", dim)
	fmt.Printf("schema ready: model=%s dimension=%d\n", model, dim)
}
