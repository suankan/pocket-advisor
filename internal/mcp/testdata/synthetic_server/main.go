package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/suankan/pocket-advisor/internal/mcp"
	"github.com/suankan/pocket-advisor/internal/retrieval"
)

type syntheticRetriever struct{}

func (syntheticRetriever) Query(ctx context.Context, req retrieval.Request) (*retrieval.Result, error) {
	if req.Question == "Wait for synthetic cancellation." {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if req.Question == "Is there absent synthetic evidence?" {
		return &retrieval.Result{
			Question: req.Question, SubQueries: []string{req.Question},
			Packets: []retrieval.Packet{}, Warnings: []string{},
			Budget: retrieval.Budget{BytesAllowed: 4096},
		}, nil
	}
	text := "The synthetic project status is ready for client compatibility testing."
	if req.Question == "Return all large synthetic evidence." {
		text = strings.Repeat("Large synthetic evidence paragraph with UTF-8 text Привет 🙂.\n\n", 2200)
	}
	return &retrieval.Result{
		Question: req.Question, SubQueries: []string{req.Question}, Warnings: []string{},
		Budget: retrieval.Budget{BytesUsed: len(text), BytesAllowed: len(text)},
		Packets: []retrieval.Packet{{
			Document: retrieval.Document{
				DocID: "11111111-1111-1111-1111-111111111111", DocType: "text",
				Title: "Synthetic project status", RawURI: "s3://synthetic/raw/status",
				SHA256: strings.Repeat("a", 64), CharCount: len(text),
			},
			Match: retrieval.Match{
				ChunkID: "22222222-2222-2222-2222-222222222222", StartByte: 0, EndByte: len(text),
				Score: 1, Legs: "both", Snippet: "Synthetic project evidence for client compatibility testing.",
			},
			Text: text, Related: []retrieval.Related{},
		}},
	}, nil
}

func main() {
	var logWriter io.Writer = os.Stderr
	if path := os.Getenv("POCKET_ADVISOR_SYNTHETIC_MCP_LOG"); path != "" {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			os.Exit(1)
		}
		defer file.Close()
		logWriter = file
	}
	server := mcp.NewServer(
		&mcp.QueryTool{
			Service: syntheticRetriever{}, Workspace: "synthetic",
			Title: "Synthetic compatibility corpus", Corpus: []string{"Synthetic project status"},
		},
		os.Stdin, os.Stdout, slog.New(slog.NewTextHandler(logWriter, nil)),
	)
	if err := server.Serve(context.Background()); err != nil {
		os.Exit(1)
	}
}
