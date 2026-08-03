package cli

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/suankan/pocket-advisor/internal/client/embedding"
	"github.com/suankan/pocket-advisor/internal/client/llm"
	"github.com/suankan/pocket-advisor/internal/client/reranking"
	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/mcp"
	"github.com/suankan/pocket-advisor/internal/retrieval"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/telemetry"
)

// runMCP serves the read path as an MCP tool over stdio.
//
// A sibling of runQuery, not a second implementation: both are thin adapters
// over the same retrieval.Query, which is why §7 requires that package to be
// transport-agnostic.
//
// This is where answer generation happens — outside this binary. The agent
// calls the tool, reads cited passages, and writes the answer. Case data
// therefore leaves the machine only when the operator puts it in a
// conversation, never as an automatic consequence of querying (§6.1).
func runMCP(o *Options, cfg *config.Config, logs *telemetry.Logs) error {
	// stdio is the protocol channel. Nothing here may print: the dashboard is
	// never started, and every log goes to a file.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log := logs.Logger(telemetry.RoleApp)

	dsn, err := cfg.WorkspacePostgresDSN(o.WorkspaceID)
	if err != nil {
		return err
	}
	db, err := postgres.Connect(ctx, dsn, cfg.Postgres.MaxConns)
	if err != nil {
		return err
	}
	defer db.Close()

	svc := retrieval.New(db,
		embedding.New(cfg.Embedding),
		reranking.New(cfg.Reranking),
		llm.New(cfg.LLM),
		cfg.Query, o.WorkspaceID, log)

	// Fail before the handshake rather than on the first search: a scope
	// violation surfaced mid-conversation would look like a bad answer.
	if err := svc.AssertScope(ctx); err != nil {
		return err
	}

	log.Info("mcp server ready", "workspace_id", o.WorkspaceID, "transport", "stdio")
	srv := mcp.NewServer(
		&mcp.QueryTool{Service: svc, Workspace: o.WorkspaceID},
		os.Stdin, os.Stdout, log)
	return srv.Serve(ctx)
}
