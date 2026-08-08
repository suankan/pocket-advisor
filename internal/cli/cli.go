// Package cli is the single entry point's flag surface and mode dispatch.
//
// Seven binaries became one. The roles did not merge — they were already
// separate packages under internal/ — only the process boundary between them
// disappeared (ingestion-design.md §8.2).
package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/telemetry"
)

// Options is the resolved command line.
type Options struct {
	ConfigPath      string
	WorkspaceConfig string
	WorkspaceID     string

	IngestAll  bool
	Scan       bool
	Reconcile  bool
	Listen     bool
	DeleteData bool
	Forget     string
	Query      string
	MCP        bool
	MCPHTTP    bool

	TopK        int
	JSON        bool
	NoRerank    bool
	NoDecompose bool
	DryRun      bool
	Yes         bool
	OCRLangs    string
	EmbedConc   int
	LogDir      string
	NoDashboard bool

	MCPHTTPAddr                  string
	MCPHTTPResourceURI           string
	MCPHTTPAuthorizationServer   string
	MCPHTTPIntrospectionEndpoint string
	MCPHTTPIntrospectionClientID string
	MCPHTTPRequiredScope         string
	MCPHTTPAllowedOrigins        string
	MCPHTTPAllowedHosts          string
	MCPHTTPTrustedProxyCIDRs     string
	MCPHTTPMaxConcurrent         int

	StaleAfter time.Duration
	HighWater  uint64
	LowWater   uint64
}

const usage = `pocket-advisor — ingestion pipeline for a local RAG corpus.

Modes (exactly one):
  --ingest-all        upload the workspace, enqueue what is new, process to completion
  --scan              enqueue Tier 1 objects with no Tier 2 row, then process
  --reconcile         re-publish documents stuck PENDING, then process
  --listen            run the pipeline indefinitely, processing RustFS's live
                      notify events as they arrive — no upload, no scan, does
                      not exit on idle. For objects arriving from something
                      other than our own uploader.
  --query <question>  ask the corpus a question and print the matching
                      sources, with citations. Returns evidence, not an
                      answer: generation happens outside this binary
                      (retrieval-design.md §6.1)
  --mcp               serve the read path as an MCP tool over stdio, so an
                      agent can search the corpus and write cited answers.
                      This is where answer generation happens — outside this
                      binary (retrieval-design.md §6.1)
  --mcp-http          serve the same fixed-workspace tools over authenticated
                      Streamable HTTP. The backend is loopback-only and must
                      sit behind the documented TLS gateway.
  --delete-data       purge the workspace from Tier 1 and Tier 2
  --forget <sha256>   remove one document by content hash

Common:
  --workspace-id <id>       workspace from the registry (required by every mode)
  --workspace-config <path> registry path override (default: infra config's
                            workspaces.config, resolved relative to it)
  --config <path>           infrastructure config (default config.yaml). Names
                            the registry and credentials files, so an absolute
                            path here is enough from any directory

Query options:
  --top-k N                 maximum results (default 15)
  --json                    machine-readable output
  --no-rerank               skip the cross-encoder; serve fused order
  --no-decompose            do not split a multi-topic question

Options:
  --yes                     skip destructive confirmation prompts
  --dry-run                 report what would be uploaded, write nothing
  --embedding-concurrency N concurrent embedding sessions (default 8)
  --ocr-langs <langs>       tesseract languages (default eng+rus)
  --log-dir <path>          per-role log files (default logs)
  --no-dashboard            plain line output instead of the live display

Authenticated HTTP MCP:
  --mcp-http-addr <addr>                 loopback backend address (default 127.0.0.1:8080)
  --mcp-http-resource-uri <https-uri>    canonical public MCP resource URI
  --mcp-http-authorization-server <uri>  OAuth authorization-server issuer
  --mcp-http-introspection-endpoint <uri> OAuth token introspection endpoint
  --mcp-http-introspection-client-id <id> resource-server introspection client
  --mcp-http-required-scope <scope>      retrieval scope (default pocket-advisor:retrieve)
  --mcp-http-allowed-origins <csv>       exact browser origins; absent Origin remains valid
  --mcp-http-allowed-hosts <csv>         exact public Host values (default resource URI host)
  --mcp-http-trusted-proxy-cidrs <csv>   peers allowed to send forwarding headers
  --mcp-http-max-concurrent N            in-flight HTTP requests (default 8)

  MCP_HTTP_INTROSPECTION_CLIENT_SECRET supplies the confidential resource-
  server credential and must come from the environment or a mounted Secret.

Every other pool is sized from the host's CPU count and is not configurable.

Examples:
  pocket-advisor --ingest-all --workspace-id test
  pocket-advisor --query "what did we agree about the school holidays?" --workspace-id test
  pocket-advisor --mcp --workspace-id test
  pocket-advisor --mcp-http --workspace-id test --mcp-http-resource-uri https://mcp.example.test/mcp ...
  pocket-advisor --delete-data --workspace-id test
`

// Parse reads the command line into Options.
func Parse(args []string) (*Options, error) {
	o := &Options{}
	fs := flag.NewFlagSet("pocket-advisor", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }

	fs.StringVar(&o.ConfigPath, "config", config.DefaultPath, "infrastructure config path")
	fs.StringVar(&o.WorkspaceConfig, "workspace-config", "", "workspace registry path override (default: infra config's workspaces.config, resolved relative to it)")
	fs.StringVar(&o.WorkspaceID, "workspace-id", "", "workspace id within the registry")

	fs.BoolVar(&o.IngestAll, "ingest-all", false, "upload, enqueue and process to completion")
	fs.BoolVar(&o.Scan, "scan", false, "enqueue un-stubbed Tier 1 objects, then process")
	fs.BoolVar(&o.Reconcile, "reconcile", false, "re-publish stalled PENDING documents, then process")
	fs.BoolVar(&o.Listen, "listen", false, "run the pipeline indefinitely on RustFS's live notify events")
	fs.BoolVar(&o.DeleteData, "delete-data", false, "purge the workspace from Tier 1 and Tier 2")
	fs.StringVar(&o.Forget, "forget", "", "remove one document by sha256")
	fs.StringVar(&o.Query, "query", "", "ask the corpus a question and print the matching sources")
	fs.BoolVar(&o.MCP, "mcp", false, "serve the read path as an MCP tool over stdio")
	fs.BoolVar(&o.MCPHTTP, "mcp-http", false, "serve the read path as authenticated Streamable HTTP MCP")

	fs.IntVar(&o.TopK, "top-k", 0, "maximum results for --query (0 = config default)")
	fs.BoolVar(&o.JSON, "json", false, "emit --query results as JSON")
	fs.BoolVar(&o.NoRerank, "no-rerank", false, "skip reranking; serve fused order (--query)")
	fs.BoolVar(&o.NoDecompose, "no-decompose", false, "do not split the question into sub-queries (--query)")

	fs.BoolVar(&o.DryRun, "dry-run", false, "report what would be uploaded, write nothing")
	fs.BoolVar(&o.Yes, "yes", false, "skip destructive confirmation prompts")
	fs.StringVar(&o.OCRLangs, "ocr-langs", "eng+rus", "tesseract languages")
	fs.IntVar(&o.EmbedConc, "embedding-concurrency", 0, "concurrent embedding sessions (0 = config default)")
	fs.StringVar(&o.LogDir, "log-dir", "", "per-role log directory (empty = config default)")
	fs.BoolVar(&o.NoDashboard, "no-dashboard", false, "plain line output instead of the live display")

	fs.StringVar(&o.MCPHTTPAddr, "mcp-http-addr", "127.0.0.1:8080", "loopback HTTP MCP backend address")
	fs.StringVar(&o.MCPHTTPResourceURI, "mcp-http-resource-uri", "", "canonical public HTTPS MCP resource URI")
	fs.StringVar(&o.MCPHTTPAuthorizationServer, "mcp-http-authorization-server", "", "OAuth authorization-server issuer")
	fs.StringVar(&o.MCPHTTPIntrospectionEndpoint, "mcp-http-introspection-endpoint", "", "OAuth token introspection endpoint")
	fs.StringVar(&o.MCPHTTPIntrospectionClientID, "mcp-http-introspection-client-id", "", "OAuth resource-server introspection client id")
	fs.StringVar(&o.MCPHTTPRequiredScope, "mcp-http-required-scope", "pocket-advisor:retrieve", "required OAuth retrieval scope")
	fs.StringVar(&o.MCPHTTPAllowedOrigins, "mcp-http-allowed-origins", "", "comma-separated exact allowed browser origins")
	fs.StringVar(&o.MCPHTTPAllowedHosts, "mcp-http-allowed-hosts", "", "comma-separated exact allowed public Host values")
	fs.StringVar(&o.MCPHTTPTrustedProxyCIDRs, "mcp-http-trusted-proxy-cidrs", "127.0.0.0/8,::1/128", "comma-separated trusted proxy CIDRs")
	fs.IntVar(&o.MCPHTTPMaxConcurrent, "mcp-http-max-concurrent", 8, "maximum concurrent authenticated HTTP MCP requests")

	fs.DurationVar(&o.StaleAfter, "stale-after", 30*time.Minute, "PENDING age that counts as stalled")
	fs.Uint64Var(&o.HighWater, "high-water", 10_000, "pause enqueueing above this many pending messages")
	fs.Uint64Var(&o.LowWater, "low-water", 2_000, "resume enqueueing below this many pending messages")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return o, o.validate()
}

func (o *Options) modes() []string {
	var m []string
	for _, c := range []struct {
		on   bool
		name string
	}{
		{o.IngestAll, "--ingest-all"},
		{o.Scan, "--scan"},
		{o.Reconcile, "--reconcile"},
		{o.Listen, "--listen"},
		{o.DeleteData, "--delete-data"},
		{o.Forget != "", "--forget"},
		{o.Query != "", "--query"},
		{o.MCP, "--mcp"},
		{o.MCPHTTP, "--mcp-http"},
	} {
		if c.on {
			m = append(m, c.name)
		}
	}
	return m
}

func (o *Options) validate() error {
	modes := o.modes()
	switch {
	case len(modes) == 0:
		return fmt.Errorf("no mode selected; pass one of --ingest-all, --scan, --reconcile, --listen, " +
			"--query, --mcp, --mcp-http, --delete-data, --forget (--help for details)")
	case len(modes) > 1:
		return fmt.Errorf("modes are mutually exclusive, got %s", strings.Join(modes, " and "))
	}

	// Every mode acts on one workspace, and a mode that silently defaulted to
	// the wrong one could purge the wrong corpus. There is no shared database
	// or bucket left to act on without one (workspace-isolation.md §13).
	if o.WorkspaceID == "" {
		return fmt.Errorf("%s requires --workspace-id", modes[0])
	}
	if o.Forget != "" && len(o.Forget) != 64 {
		return fmt.Errorf("--forget expects a 64-character sha256, got %d characters", len(o.Forget))
	}
	return nil
}

// NeedsPipeline reports whether this mode enqueues work and must therefore also
// drain it. In a monolith an enqueueing mode that did not run the pools would
// publish into a void: nothing else is listening.
func (o *Options) NeedsPipeline() bool {
	return o.IngestAll || o.Scan || o.Reconcile || o.Listen
}

// Mode is the selected mode's name, for the dashboard header.
func (o *Options) Mode() string {
	if m := o.modes(); len(m) > 0 {
		return strings.TrimPrefix(m[0], "--")
	}
	return "idle"
}

// Run dispatches to the selected mode.
func Run(o *Options) error {
	cfg, err := config.Load(o.ConfigPath)
	if err == nil {
		// Reconcile the flag and the config onto one path, in both directions.
		// The flag previously reached only the two modes that passed it to
		// workspace.Load directly; anything resolving through cfg.Workspace
		// kept the config's own value, so --workspace-config silently did
		// nothing for most modes. That is invisible when the process runs from
		// the repository root and fatal when it does not — which is exactly
		// how an MCP client launches it.
		//
		// The flag is an override now rather than a necessity: config.Load
		// anchors both workspace paths to the directory of the config file that
		// named them, so --config alone locates the registry and the
		// credentials file from any working directory (deviation 26).
		if o.WorkspaceConfig != "" {
			cfg.WorkspacesConfigPath = o.WorkspaceConfig
		} else {
			o.WorkspaceConfig = cfg.WorkspacesConfigPath
		}
	}
	if err != nil {
		return err
	}
	if o.LogDir != "" {
		cfg.LogDir = o.LogDir
	}
	if o.EmbedConc > 0 {
		cfg.Embedding.Concurrency = o.EmbedConc
	}

	// The read path logs to stderr rather than to per-role files. Those files
	// exist to separate five concurrent worker pools during an ingest; a query
	// has no pools, and an MCP server's stderr is captured by the client that
	// launched it, which is where anyone debugging a failed server looks.
	//
	// It also has to be this way: a client launches the server from a working
	// directory it may not be able to write to, and creating a relative "logs"
	// directory there fails outright — which is precisely how the first
	// Claude Desktop attempt died, before the handshake and with the reason
	// only visible in the client's own log.
	var logs *telemetry.Logs
	if o.MCP || o.MCPHTTP || o.Query != "" {
		logs = telemetry.StderrLogs(cfg.LogLevel)
	} else {
		logs, err = telemetry.OpenLogs(cfg.LogDir, cfg.LogLevel)
		if err != nil {
			return err
		}
	}
	defer logs.Close()

	switch {
	case o.DeleteData, o.Forget != "":
		return runReset(o, cfg, logs)
	case o.Query != "":
		return runQuery(o, cfg, logs)
	case o.MCP:
		return runMCP(o, cfg, logs)
	case o.MCPHTTP:
		return runMCPHTTP(o, cfg, logs)
	case o.Listen:
		return runListen(o, cfg, logs)
	default:
		return runIngest(o, cfg, logs)
	}
}

// interrupts wires the two-stage stop.
//
// The first signal stops fetching but lets in-flight handlers finish and ack.
// Abandoning them unacked would burn one of their three delivery attempts, so
// three interrupted runs would dead-letter documents that were never broken.
// The second signal, or a drain that overruns, cancels the work itself.
func interrupts(parent context.Context, log interface{ Info(string, ...any) }) (fetchCtx, workCtx context.Context, stop func()) {
	fetch, stopFetch := context.WithCancel(parent)
	work, stopWork := context.WithCancel(parent)

	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	done := make(chan struct{})
	go func() {
		defer signal.Stop(sigs)
		select {
		case <-done:
			return
		case <-sigs:
		}
		log.Info("interrupt: draining in-flight work, press Ctrl+C again to abort")
		stopFetch()

		select {
		case <-done:
		case <-sigs:
			log.Info("second interrupt: aborting in-flight work")
			stopWork()
		case <-time.After(drainGrace):
			log.Info("drain grace elapsed, aborting in-flight work")
			stopWork()
		}
	}()

	// Idempotent: callers both defer this and call it explicitly once the
	// pipeline has drained, and a second close of done would panic.
	var once sync.Once
	return fetch, work, func() {
		once.Do(func() {
			close(done)
			stopFetch()
			stopWork()
		})
	}
}

// drainGrace bounds how long a graceful stop waits. Comfortably under the
// broker's 5-minute AckWait, so anything still running when it elapses would
// have been redelivered anyway.
const drainGrace = 2 * time.Minute

// confirm gates a destructive action on stdin.
func confirm(prompt string) bool {
	fmt.Printf("%s\nType 'yes' to continue: ", prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	return strings.TrimSpace(strings.ToLower(line)) == "yes"
}

// errAborted is returned when the operator declines a confirmation.
var errAborted = errors.New("aborted")
