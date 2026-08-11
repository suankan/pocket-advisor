package cli

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/suankan/pocket-advisor/internal/client/embedding"
	"github.com/suankan/pocket-advisor/internal/client/llm"
	"github.com/suankan/pocket-advisor/internal/client/reranking"
	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/mailbox"
	"github.com/suankan/pocket-advisor/internal/mcp"
	"github.com/suankan/pocket-advisor/internal/retrieval"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/telemetry"
	"github.com/suankan/pocket-advisor/internal/workspace"
)

func newMCPTool(ctx context.Context, o *Options, cfg *config.Config, logs *telemetry.Logs) (*mcp.QueryTool, *retrieval.Service, func(), error) {
	log := logs.Logger(telemetry.RoleApp)
	// Resolve the one selected workspace before constructing either service.
	// Owner identities stay private in the registry and are passed only to the
	// fixed-scope mailbox service; MCP arguments cannot replace them.
	resolved, err := workspace.Load(o.WorkspaceConfig, o.WorkspaceID)
	if err != nil {
		return nil, nil, nil, err
	}
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
	mailboxService, err := mailbox.NewWithOwnerIdentities(mailbox.NewPostgresStore(db), o.WorkspaceID, resolved.OwnerIdentities, mailbox.DefaultConfig(), log)
	if err != nil {
		db.Close()
		return nil, nil, nil, err
	}
	title, corpus := describeCorpus(o)
	mailboxTool := &mcp.MailboxTool{Service: mailboxService, Workspace: o.WorkspaceID, Title: title}
	return &mcp.QueryTool{Service: svc, Workspace: o.WorkspaceID, Title: title, Corpus: corpus, Mailbox: mailboxTool}, svc, db.Close, nil
}

func assertMCPEndpoints(ctx context.Context, cfg *config.Config) error {
	endpoints := []string{cfg.Embedding.Endpoint}
	if cfg.Query.RerankEnabled {
		endpoints = append(endpoints, cfg.Reranking.Endpoint)
	}
	if cfg.Query.DecomposeEnabled {
		endpoints = append(endpoints, cfg.LLM.Endpoint)
	}
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

// mcpStdioCmd serves the read path as an MCP tool over stdio.
//
// A sibling of mcpStartCmd, not a second implementation: both are thin
// adapters over the same retrieval.Query, which is why §7 requires that
// package to be transport-agnostic.
//
// This is where answer generation happens — outside this binary. The agent
// calls the tool, reads cited passages, and writes the answer. Case data
// therefore leaves the machine only when the operator puts it in a
// conversation, never as an automatic consequence of querying (§6.1).
func mcpStdioCmd(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("mcp stdio", flag.ExitOnError)
	workspaceID := fs.String("workspace-id", "", "workspace to serve")
	workspaceConfig := fs.String("workspace-config", "", "workspace registry path override (default from config)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *workspaceID == "" {
		return fmt.Errorf("--workspace-id is required")
	}
	wsConfig := cfg.WorkspacesConfigPath
	if *workspaceConfig != "" {
		wsConfig = *workspaceConfig
	}

	// stdio is the protocol channel. Nothing here may print: the dashboard is
	// never started, and every log goes to a file.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	logs := telemetry.StderrLogs(cfg.LogLevel)
	defer logs.Close()
	log := logs.Logger(telemetry.RoleApp)

	tool, _, closeDB, err := newMCPTool(ctx, &Options{WorkspaceID: *workspaceID, WorkspaceConfig: wsConfig}, cfg, logs)
	if err != nil {
		return err
	}
	defer closeDB()

	log.Info("mcp server ready", "workspace_id", *workspaceID, "transport", "stdio", "collections", len(tool.Corpus))
	srv := mcp.NewServer(tool, os.Stdin, os.Stdout, log)
	return srv.Serve(ctx)
}

// mcpStartCmd starts the MCP HTTP server. Authentication is optional: leave
// --google-client-id/--allowed-emails (and config.yaml's mcp.oauth) unset to
// serve unauthenticated on loopback, for local development.
func mcpStartCmd(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("mcp start", flag.ExitOnError)
	workspaceID := fs.String("workspace-id", "", "workspace to serve")
	addr := fs.String("addr", "", "listen address (default from config)")
	resourceURI := fs.String("resource-uri", "", "public MCP resource URI (default from config)")
	googleClientID := fs.String("google-client-id", "", "Google OAuth client ID (default from config)")
	allowedEmails := fs.String("allowed-emails", "", "comma-separated Google account emails allowed to authenticate (default from config)")
	certFile := fs.String("cert-file", "", "TLS certificate file (default from config)")
	keyFile := fs.String("key-file", "", "TLS key file (default from config)")
	allowedOrigins := fs.String("allowed-origins", "", "comma-separated exact allowed browser origins")
	allowedHosts := fs.String("allowed-hosts", "", "comma-separated exact allowed public Host values (default: resource URI host)")
	trustedProxyCIDRs := fs.String("trusted-proxy-cidrs", "", "comma-separated trusted proxy CIDRs")
	maxConcurrent := fs.Int("max-concurrent", 0, "maximum concurrent HTTP requests (default from config)")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if *workspaceID == "" {
		return fmt.Errorf("--workspace-id is required")
	}

	// Apply flag overrides to config
	if *addr != "" {
		cfg.MCP.HTTP.Addr = *addr
	}
	if *resourceURI != "" {
		cfg.MCP.HTTP.ResourceURI = *resourceURI
	}
	if *googleClientID != "" {
		cfg.MCP.OAuth.GoogleClientID = *googleClientID
	}
	if *allowedEmails != "" {
		cfg.MCP.OAuth.AllowedEmails = splitCSV(*allowedEmails)
	}
	if *certFile != "" {
		cfg.MCP.TLS.CertFile = *certFile
	}
	if *keyFile != "" {
		cfg.MCP.TLS.KeyFile = *keyFile
	}
	if *maxConcurrent > 0 {
		cfg.MCP.HTTP.MaxConcurrent = *maxConcurrent
	}

	// Set defaults
	if cfg.MCP.HTTP.Addr == "" {
		cfg.MCP.HTTP.Addr = "127.0.0.1:8080"
	}
	if cfg.MCP.HTTP.Endpoint == "" {
		cfg.MCP.HTTP.Endpoint = "/mcp"
	}

	// Derive resource_uri from addr if not set. Google auth requires an HTTPS
	// resource URI (internal/mcp.normalizeHTTPOptions enforces this), so this
	// plain-HTTP default only actually serves the common unauthenticated-local
	// case; an operator turning on Google auth must set resource_uri (and
	// terminate TLS via cert/key or a reverse proxy) explicitly.
	if cfg.MCP.HTTP.ResourceURI == "" {
		cfg.MCP.HTTP.ResourceURI = "http://" + cfg.MCP.HTTP.Addr + cfg.MCP.HTTP.Endpoint
	}

	logs := telemetry.StderrLogs(cfg.LogLevel)
	defer logs.Close()
	log := logs.Logger(telemetry.RoleApp)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	tool, svc, closeDB, err := newMCPTool(ctx, &Options{
		WorkspaceID:     *workspaceID,
		WorkspaceConfig: cfg.WorkspacesConfigPath,
	}, cfg, logs)
	if err != nil {
		return err
	}
	defer closeDB()

	pidPath := mcpPIDPath(cfg, *workspaceID)
	if pid, alive := readLivePID(pidPath); alive {
		return fmt.Errorf("mcp server for workspace %q is already running (pid %d); stop it first", *workspaceID, pid)
	}
	if err := writePIDFile(pidPath); err != nil {
		return fmt.Errorf("write pid file: %w", err)
	}
	defer os.Remove(pidPath)

	server, err := mcp.NewHTTPServer(ctx, tool, mcp.HTTPOptions{
		Address: cfg.MCP.HTTP.Addr, Endpoint: cfg.MCP.HTTP.Endpoint, ResourceURI: cfg.MCP.HTTP.ResourceURI,
		GoogleClientID: cfg.MCP.OAuth.GoogleClientID, AllowedEmails: cfg.MCP.OAuth.AllowedEmails,
		CertFile: cfg.MCP.TLS.CertFile, KeyFile: cfg.MCP.TLS.KeyFile,
		AllowedOrigins: splitCSV(*allowedOrigins), AllowedHosts: splitCSV(*allowedHosts),
		TrustedProxyCIDRs: splitCSV(*trustedProxyCIDRs), MaxConcurrent: cfg.MCP.HTTP.MaxConcurrent,
		Readiness: func(readyCtx context.Context) error {
			if err := svc.AssertScope(readyCtx); err != nil {
				return fmt.Errorf("workspace database unavailable")
			}
			return assertMCPEndpoints(readyCtx, cfg)
		},
	})
	if err != nil {
		return fmt.Errorf("configure HTTP MCP: %w", err)
	}

	log.Info("mcp server ready",
		"transport", "streamable_http",
		"workspace", *workspaceID,
		"addr", cfg.MCP.HTTP.Addr,
		"resource_uri", cfg.MCP.HTTP.ResourceURI,
		"authenticated", cfg.MCP.OAuth.GoogleClientID != "",
		"collections", len(tool.Corpus))

	return server.Serve(ctx)
}

// mcpStopCmd stops a running MCP HTTP server for the given workspace.
func mcpStopCmd(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("mcp stop", flag.ExitOnError)
	workspaceID := fs.String("workspace-id", "", "workspace whose server to stop")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *workspaceID == "" {
		return fmt.Errorf("--workspace-id is required")
	}

	path := mcpPIDPath(cfg, *workspaceID)
	pid, alive := readLivePID(path)
	if !alive {
		fmt.Printf("mcp server for workspace %q is not running.\n", *workspaceID)
		return nil
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal process %d: %w", pid, err)
	}

	// mcpStartCmd removes its own PID file as part of graceful shutdown once
	// its context is cancelled by this same SIGTERM, so polling for the file
	// (or the process) to go away is polling for confirmed exit, not guessing.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, stillAlive := readLivePID(path); !stillAlive {
			fmt.Printf("mcp server for workspace %q stopped (pid %d).\n", *workspaceID, pid)
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("mcp server for workspace %q (pid %d) did not stop within 5s", *workspaceID, pid)
}

// mcpStatusCmd reports whether a workspace's MCP HTTP server is running.
func mcpStatusCmd(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("mcp status", flag.ExitOnError)
	workspaceID := fs.String("workspace-id", "", "workspace whose server status to check")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *workspaceID == "" {
		return fmt.Errorf("--workspace-id is required")
	}

	pid, alive := readLivePID(mcpPIDPath(cfg, *workspaceID))
	if !alive {
		fmt.Printf("mcp server for workspace %q: not running.\n", *workspaceID)
		return nil
	}
	fmt.Printf("mcp server for workspace %q: running (pid %d).\n", *workspaceID, pid)
	return nil
}

// mcpPIDPath is where mcpStartCmd records its process id so mcp stop/status
// can find it later. It lives next to the per-role logs (cfg.LogDir is
// already anchored to config.yaml's directory, not the process's cwd — see
// config.applyFile), keyed by workspace so more than one workspace's server
// can run at once.
func mcpPIDPath(cfg *config.Config, workspaceID string) string {
	return filepath.Join(cfg.LogDir, "mcp-"+workspaceID+".pid")
}

func writePIDFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644)
}

// readLivePID reads path and reports the pid it names only if that process
// is still alive. A pid file left behind by a killed-not-stopped server is
// stale; it is removed here so a later mcp start is not permanently refused.
func readLivePID(path string) (pid int, alive bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return 0, false
	}
	// Signal 0 checks liveness without actually sending a signal (POSIX
	// kill(2); os.FindProcess on Unix always succeeds regardless of whether
	// the pid exists, so this is the actual liveness check).
	if err := process.Signal(syscall.Signal(0)); err != nil {
		_ = os.Remove(path)
		return 0, false
	}
	return pid, true
}
