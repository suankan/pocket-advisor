package cli

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

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

	tool, _, closeDB, err := newMCPTool(ctx, o, cfg, logs)
	if err != nil {
		return err
	}
	defer closeDB()
	log := logs.Logger(telemetry.RoleApp)

	log.Info("mcp server ready", "workspace_id", o.WorkspaceID,
		"transport", "stdio", "collections", len(tool.Corpus))
	srv := mcp.NewServer(tool, os.Stdin, os.Stdout, log)
	return srv.Serve(ctx)
}

// runMCPHTTP serves the same tool through a loopback-only backend. The public
// socket belongs to the Caddy gateway, which terminates TLS before forwarding
// into the pod's shared loopback namespace. OAuth is still enforced here at
// the resource server so a gateway routing mistake cannot bypass identity.
func runMCPHTTP(o *Options, cfg *config.Config, logs *telemetry.Logs) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	tool, svc, closeDB, err := newMCPTool(ctx, o, cfg, logs)
	if err != nil {
		return err
	}
	defer closeDB()

	secret := os.Getenv("MCP_HTTP_INTROSPECTION_CLIENT_SECRET")
	server, err := mcp.NewHTTPServer(tool, mcp.HTTPOptions{
		Address: o.MCPHTTPAddr, ResourceURI: o.MCPHTTPResourceURI,
		AuthorizationServer:   o.MCPHTTPAuthorizationServer,
		IntrospectionEndpoint: o.MCPHTTPIntrospectionEndpoint,
		IntrospectionClientID: o.MCPHTTPIntrospectionClientID,
		IntrospectionSecret:   secret, RequiredScope: o.MCPHTTPRequiredScope,
		AllowedOrigins: splitCSV(o.MCPHTTPAllowedOrigins), AllowedHosts: splitCSV(o.MCPHTTPAllowedHosts),
		TrustedProxyCIDRs: splitCSV(o.MCPHTTPTrustedProxyCIDRs), MaxConcurrent: o.MCPHTTPMaxConcurrent,
		Readiness: func(readyCtx context.Context) error {
			if err := svc.AssertScope(readyCtx); err != nil {
				return fmt.Errorf("workspace database unavailable")
			}
			return assertMCPEndpoints(readyCtx, cfg, o.MCPHTTPIntrospectionEndpoint)
		},
	})
	if err != nil {
		return fmt.Errorf("configure HTTP MCP: %w", err)
	}
	logs.Logger(telemetry.RoleApp).Info("mcp server ready",
		"transport", "streamable_http", "collections", len(tool.Corpus))
	return server.Serve(ctx)
}

func newMCPTool(ctx context.Context, o *Options, cfg *config.Config, logs *telemetry.Logs) (*mcp.QueryTool, *retrieval.Service, func(), error) {
	log := logs.Logger(telemetry.RoleApp)
	dsn, err := cfg.WorkspacePostgresDSN(o.WorkspaceID)
	if err != nil {
		return nil, nil, nil, err
	}
	db, err := postgres.Connect(ctx, dsn, cfg.Postgres.MaxConns)
	if err != nil {
		return nil, nil, nil, err
	}
	svc := retrieval.New(db,
		embedding.New(cfg.Embedding),
		reranking.New(cfg.Reranking),
		llm.New(cfg.LLM),
		cfg.Query, o.WorkspaceID, log)
	if err := svc.AssertScope(ctx); err != nil {
		db.Close()
		return nil, nil, nil, err
	}
	title, corpus := describeCorpus(o)
	return &mcp.QueryTool{Service: svc, Workspace: o.WorkspaceID, Title: title, Corpus: corpus}, svc, db.Close, nil
}

func assertMCPEndpoints(ctx context.Context, cfg *config.Config, introspectionEndpoint string) error {
	endpoints := []string{cfg.Embedding.Endpoint}
	if cfg.Query.RerankEnabled {
		endpoints = append(endpoints, cfg.Reranking.Endpoint)
	}
	if cfg.Query.DecomposeEnabled {
		endpoints = append(endpoints, cfg.LLM.Endpoint)
	}
	endpoints = append(endpoints, introspectionEndpoint)
	for _, endpoint := range endpoints {
		u, err := url.Parse(endpoint)
		if err != nil || u.Hostname() == "" {
			return fmt.Errorf("required HTTP dependency endpoint is invalid")
		}
		port := u.Port()
		if port == "" {
			switch u.Scheme {
			case "https":
				port = "443"
			case "http":
				port = "80"
			default:
				return fmt.Errorf("required HTTP dependency endpoint is invalid")
			}
		}
		connection, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(u.Hostname(), port))
		if err != nil {
			return fmt.Errorf("required HTTP dependency endpoint is unavailable")
		}
		_ = connection.Close()
	}
	return nil
}

func splitCSV(raw string) []string {
	var out []string
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
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
