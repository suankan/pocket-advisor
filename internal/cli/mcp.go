package cli

import (
	"context"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/suankan/pocket-advisor/internal/client/embedding"
	"github.com/suankan/pocket-advisor/internal/client/llm"
	"github.com/suankan/pocket-advisor/internal/client/reranking"
	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/mcp"
	"github.com/suankan/pocket-advisor/internal/retrieval"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/telemetry"
	"github.com/suankan/pocket-advisor/internal/workspace"
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

	// Describe the corpus from the registry rather than by its workspace id.
	// With several servers registered, the agent chooses between them on the
	// tool description alone, and "case-documents-demo" tells it nothing about
	// whether that is where the bank statements live.
	title, corpus := describeCorpus(o)

	log.Info("mcp server ready", "workspace_id", o.WorkspaceID,
		"transport", "stdio", "collections", len(corpus))
	srv := mcp.NewServer(
		&mcp.QueryTool{Service: svc, Workspace: o.WorkspaceID, Title: title, Corpus: corpus},
		os.Stdin, os.Stdout, log)
	return srv.Serve(ctx)
}

// describeCorpus renders the workspace registry as human-readable contents.
//
// Failure is deliberately soft: an undescribed tool is worse than an
// unstartable server, so a registry that cannot be read costs the description
// and nothing else.
func describeCorpus(o *Options) (title string, corpus []string) {
	ws, err := workspace.Load(o.WorkspaceConfig, o.WorkspaceID)
	if err != nil {
		return "", nil
	}
	for _, c := range ws.Collections {
		switch {
		case c.Title != "":
			corpus = append(corpus, c.Title)
		case c.AccountNumber != "":
			// Bank collections carry no title but do carry the account it
			// covers, which is exactly what makes them selectable.
			label := strings.ReplaceAll(c.ID, "-", " ")
			corpus = append(corpus, label+" (account "+c.AccountNumber+")")
		default:
			corpus = append(corpus, strings.ReplaceAll(c.ID, "-", " "))
		}
	}
	return ws.Title, corpus
}
