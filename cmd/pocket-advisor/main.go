// Command pocket-advisor is the ingestion pipeline.
//
// It runs on the host against three stores in the local cluster (RustFS,
// PostgreSQL+pgvector, NATS JetStream), which communicate as before over
// JetStream subjects carrying protobuf commands. What changed is that every
// worker role now runs as a pool of goroutines in this process rather than as
// its own Deployment (ingestion-design.md §5, §6.2).
//
//	pocket-advisor --ingest-all --workspace-id test
//	pocket-advisor --delete-data --workspace-id test
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/suankan/pocket-advisor/internal/cli"
)

func main() {
	opts, err := cli.Parse(os.Args[1:])
	if err != nil {
		// flag's own errors are already printed, along with the usage text.
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(os.Stderr, "pocket-advisor: %v\n", err)
		}
		os.Exit(2)
	}

	if err := cli.Run(opts); err != nil {
		fmt.Fprintf(os.Stderr, "pocket-advisor: %v\n", err)
		os.Exit(1)
	}
}
