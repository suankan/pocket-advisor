package cli

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
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
	"github.com/suankan/pocket-advisor/internal/topicgraph"
	"github.com/suankan/pocket-advisor/internal/workspace"
)

func newMCPTool(ctx context.Context, o *Options, cfg *config.Config, logs *telemetry.Logs) (*mcp.QueryTool, *retrieval.Service, func(), error) {
	log := logs.Logger(telemetry.RoleMCP)
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
	mailboxService, err := mailbox.NewWithOwnerIdentities(mailbox.NewPostgresStore(db), resolved.OwnerIdentities, mailbox.DefaultConfig(), log)
	if err != nil {
		db.Close()
		return nil, nil, nil, err
	}
	timelineService, err := topicgraph.NewTimelineService(postgres.NewTopicTimelineStore(db))
	if err != nil {
		db.Close()
		return nil, nil, nil, err
	}
	title, corpus := describeCorpus(o)
	mailboxTool := &mcp.MailboxTool{Service: mailboxService, Workspace: o.WorkspaceID, Title: title, Log: log}
	timelineTool := &mcp.TimelineTool{Service: timelineService, Workspace: o.WorkspaceID, Title: title, Log: log}
	documentTool := &mcp.DocumentTool{Docs: postgres.NewDocumentRepo(db), Workspace: o.WorkspaceID, Title: title, Log: log}
	return &mcp.QueryTool{Service: svc, Workspace: o.WorkspaceID, Title: title, Corpus: corpus, Mailbox: mailboxTool, Timeline: timelineTool, Document: documentTool, Log: log}, svc, db.Close, nil
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
//
// A workspace no longer carries per-directory registry descriptions — it is
// one recursively walked tree with no further subdivision (§3.1) — so the
// contents hint is instead the workspace's own top-level directory names, a
// cheap single-level read rather than the full recursive walk ingestion
// itself performs.
func describeCorpus(o *Options) (title string, corpus []string) {
	ws, err := workspace.Load(o.WorkspaceConfig, o.WorkspaceID)
	if err != nil {
		return "", nil
	}
	entries, err := os.ReadDir(ws.AbsPath)
	if err != nil {
		return ws.Title, nil
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		corpus = append(corpus, strings.ReplaceAll(e.Name(), "-", " "))
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

	// stdio is the protocol channel. Nothing here may print to stdout: the
	// dashboard is never started, and stdout is reserved for JSON-RPC frames.
	// Logs go to both logs/mcp.log (RoleMCP, the same file mcp start writes
	// to) and stderr, which an MCP client typically captures into its own log
	// view — the durable record and the client-visible one, at once.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	logs, err := telemetry.OpenLogsTeeStderr(cfg.LogDir, cfg.LogLevel)
	if err != nil {
		return err
	}
	defer logs.Close()
	log := logs.Logger(telemetry.RoleMCP)

	tool, _, closeDB, err := newMCPTool(ctx, &Options{WorkspaceID: *workspaceID, WorkspaceConfig: wsConfig}, cfg, logs)
	if err != nil {
		return err
	}
	defer closeDB()

	log.Info("mcp server ready", "workspace_id", *workspaceID, "transport", "stdio", "corpus_entries", len(tool.Corpus))
	srv := mcp.NewServer(tool, os.Stdin, os.Stdout, log)
	return srv.Serve(ctx)
}

// mcpDaemonEnvVar selects the daemon child's own code path when mcp start
// re-execs this same binary. Its presence, not its value, matters: the
// wrapper never runs with it set, and the child never runs without it.
const mcpDaemonEnvVar = "POCKET_ADVISOR_MCP_DAEMON"

// mcpStartCmd starts the MCP HTTP server as a detached background daemon,
// operated afterward through mcp stop/status, and returns once that daemon is
// confirmed running. The invoking process (the wrapper) forks a child that
// re-execs this same binary with mcpDaemonEnvVar set; that env var, inherited
// by the child alone, is what selects runMCPHTTPServer over forking again.
func mcpStartCmd(cfg *config.Config, configPath string, args []string) error {
	if os.Getenv(mcpDaemonEnvVar) == "1" {
		return runMCPHTTPServer(cfg, args)
	}
	return daemonizeMCPStart(cfg, configPath, args)
}

// daemonizeMCPStart is the wrapper half of mcp start: it fails fast on an
// already-running daemon, forks and detaches the actual server, and waits for
// that child to confirm it is alive before returning — so a caller never sees
// a false "started" for a child that immediately exited on a bad flag or an
// unreachable database.
func daemonizeMCPStart(cfg *config.Config, configPath string, args []string) error {
	workspaceID := scanFlagValue(args, "workspace-id")
	if workspaceID == "" {
		return fmt.Errorf("--workspace-id is required")
	}
	pidPath := mcpPIDPath(cfg, workspaceID)
	if pid, alive := readLivePID(pidPath); alive {
		return fmt.Errorf("mcp server for workspace %q is already running (pid %d); stop it first", workspaceID, pid)
	}

	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	absConfigPath, err := resolveConfigPath(configPath)
	if err != nil {
		return fmt.Errorf("resolve config path: %w", err)
	}

	logPath := filepath.Join(cfg.LogDir, telemetry.RoleMCP+".log")
	if err := os.MkdirAll(cfg.LogDir, 0o755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(execPath, daemonChildArgs(absConfigPath, args)...)
	cmd.Env = append(os.Environ(), mcpDaemonEnvVar+"=1")
	// The child's own structured logging (RoleMCP, opened independently
	// inside runMCPHTTPServer) already goes to logPath. Redirecting the raw
	// process streams here too catches anything outside that — a panic before
	// the logger opens, or output from a linked C library — so nothing a
	// detached daemon produces is ever silently lost.
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Setsid detaches the child from this process's controlling terminal and
	// session, so it keeps running (and does not receive a SIGHUP) after this
	// wrapper exits — a real background daemon, not a job an operator has to
	// remember to disown or nohup themselves.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start mcp daemon: %w", err)
	}
	// This process never waits for the daemon to exit; Release drops Go's own
	// bookkeeping for that child rather than leaving a Wait no caller will
	// ever make.
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("release mcp daemon: %w", err)
	}

	// Confirm the daemon reached the point of writing its own PID file — proof
	// it opened its logs, resolved the workspace, and connected to the
	// database, not merely that the OS accepted fork+exec — before reporting
	// success. A daemon that dies immediately after (a bad flag, an
	// unreachable database) is diagnosed from logPath, which already holds its
	// output.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pid, alive := readLivePID(pidPath); alive {
			fmt.Printf("mcp server for workspace %q started (pid %d); logging to %s\n", workspaceID, pid, logPath)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("mcp server for workspace %q did not confirm startup within 5s; see %s", workspaceID, logPath)
}

// daemonChildArgs rebuilds the daemon child's command line: the original
// mcp start arguments, prefixed with an explicit absolute --config so the
// child's own top-level flag parsing resolves the same configuration
// regardless of its working directory once detached.
func daemonChildArgs(absConfigPath string, args []string) []string {
	childArgs := make([]string, 0, len(args)+4)
	childArgs = append(childArgs, "mcp", "start", "--config", absConfigPath)
	return append(childArgs, args...)
}

// resolveConfigPath applies the same empty-path default config.Load does,
// then resolves it to an absolute path, so the forked child depends on
// neither its parent's working directory (should the two ever differ once
// detached) nor an inherited relative path.
func resolveConfigPath(configPath string) (string, error) {
	if configPath == "" {
		configPath = config.DefaultPath
	}
	return filepath.Abs(configPath)
}

// scanFlagValue reads one flag's value out of an unparsed argument list
// without declaring the rest of the flag surface, which the wrapper does not
// need and must not have to keep in sync with runMCPHTTPServer's full set. It
// accepts a single or double dash and either "-name value" or "-name=value",
// matching what Go's own flag package accepts.
func scanFlagValue(args []string, name string) string {
	oneDash, twoDash := "-"+name, "--"+name
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == oneDash || arg == twoDash:
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		case strings.HasPrefix(arg, oneDash+"="):
			return strings.TrimPrefix(arg, oneDash+"=")
		case strings.HasPrefix(arg, twoDash+"="):
			return strings.TrimPrefix(arg, twoDash+"=")
		}
	}
	return ""
}

// runMCPHTTPServer is the daemon child's actual server. It is a sibling of
// mcpStdioCmd, not a second implementation: both are thin adapters over the
// same retrieval.Query, which is why §7 requires that package to be
// transport-agnostic. Authentication is optional: leave
// --google-client-id/--allowed-emails (and config.yaml's mcp.oauth) unset to
// serve unauthenticated on loopback, for local development.
func runMCPHTTPServer(cfg *config.Config, args []string) error {
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

	logs, err := telemetry.OpenLogs(cfg.LogDir, cfg.LogLevel)
	if err != nil {
		return err
	}
	defer logs.Close()
	log := logs.Logger(telemetry.RoleMCP)

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
		"corpus_entries", len(tool.Corpus))

	return server.Serve(ctx)
}

// mcpStopCmd stops a running MCP HTTP daemon for the given workspace,
// signalling the exact process mcp start's wrapper confirmed and recorded.
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

	// runMCPHTTPServer removes its own PID file as part of graceful shutdown
	// once its context is cancelled by this same SIGTERM, so polling for the
	// file (or the process) to go away is polling for confirmed exit, not
	// guessing.
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

// mcpPIDPath is where runMCPHTTPServer (the daemon child mcp start forks and
// detaches) records its process id, so mcp stop/status can find it later. It
// lives next to the per-role logs (cfg.LogDir is already anchored to
// config.yaml's directory, not the process's cwd — see config.applyFile),
// keyed by workspace so more than one workspace's server can run at once.
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
