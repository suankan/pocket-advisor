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
	SubCommand      string   // "mcp stdio", "mcp start", "mcp stop", "mcp status"
	SubArgs         []string // Arguments after subcommand
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

	Doctor  bool
	Recover bool

	// Email metadata reprocessing (ingestion-design.md §2.5): a maintenance
	// walk over one workspace's email message documents, not an ingest.
	ReprocessEmail   bool
	ReprocessLimit   int
	ReprocessConc    int
	ReprocessMissing bool

	Evaluate       bool
	EvalCases      string
	EvalReport     string
	EvalFilterIDs  string
	EvalFilterCats string
	EvalRunHNSW    bool
	EvalEfSearch   int
	EvalReadiness  bool
	EvalThresholds string

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
  --doctor            read-only workspace health checks
  --recover           plan and optionally apply ingestion recovery
                      (default: dry-run; use --yes to apply)
  --reprocess-email-metadata
                      rebuild durable email browse metadata from Tier 1 for
                      every email message document in the workspace. For a
                      workspace ingested before those tables existed: the
                      schema upgrade adds them empty and synthesises nothing.
                      Reads objects and writes only the email metadata tables —
                      no upload, no queue, no re-extraction, no re-embedding —
                      and is idempotent, so repeating an interrupted run
                      converges (ingestion-design.md §2.5)
  --evaluate          run retrieval quality evaluation against evaluation cases.
                      Uses workspaces/evaluation/<workspace>/cases.json by default.
                      --json emits machine-readable output.
  --delete-data       purge the workspace from Tier 1 and Tier 2
  --forget <sha256>   remove one document by content hash

MCP (a subcommand, not a mode flag):
  mcp stdio --workspace-id <id>
                      serve the read path as an MCP tool over stdio, so an
                      agent can search the corpus and write cited answers.
                      This is where answer generation happens — outside this
                      binary (retrieval-design.md §6.1)
  mcp start --workspace-id <id> [options]
                      serve the same fixed-workspace tools over Streamable
                      HTTP, on loopback by default. Optionally authenticated
                      against Google as the sole identity provider (config.yaml's
                      mcp.oauth: google_client_id + allowed_emails); omit both
                      to run unauthenticated for local development. See
                      docs/mcp.md.
  mcp stop --workspace-id <id>
                      stop a running "mcp start" server for that workspace
  mcp status --workspace-id <id>
                      report whether that workspace's HTTP server is running

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

Email metadata reprocessing options:
  --reprocess-limit N        stop after this many documents (0 = the whole
                             workspace)
  --reprocess-concurrency N  concurrent Tier 1 reads and metadata writes
                             (default 4)
  --reprocess-missing-only   only documents that have no metadata row yet
  --dry-run                  read and parse, report the counts, write nothing
  --json                     machine-readable summary

Evaluate options:
  --eval-cases <path>        evaluation case set JSON (default: workspaces/evaluation/<workspace>/cases.json)
  --eval-report <path>       write report to path (gitignored, default: none)
  --eval-filter-ids <csv>    comma-separated case IDs to evaluate
  --eval-filter-cats <csv>   comma-separated categories to evaluate
  --eval-hnsw                run exact vs HNSW dense search comparison
  --eval-ef-search N         HNSW ef_search for comparison (default 40)
  --eval-readiness           check readiness without running queries
  --eval-thresholds <path>   thresholds JSON (default: built-in thresholds)

Options:
  --yes                     skip destructive confirmation prompts
  --dry-run                 report what would be uploaded, write nothing
  --embedding-concurrency N concurrent embedding sessions (default 8)
  --ocr-langs <langs>       tesseract languages (default eng+rus)
  --log-dir <path>          per-role log files (default logs)
  --no-dashboard            plain line output instead of the live display

Every other pool is sized from the host's CPU count and is not configurable.

Examples:
  pocket-advisor --ingest-all --workspace-id test
  pocket-advisor --query "what did we agree about the school holidays?" --workspace-id test
  pocket-advisor mcp stdio --workspace-id test
  pocket-advisor mcp start --workspace-id test
  pocket-advisor --reprocess-email-metadata --workspace-id test --dry-run
  pocket-advisor --delete-data --workspace-id test
`

// Parse reads the command line into Options.
func Parse(args []string) (*Options, error) {
	o := &Options{}

	// Check for subcommands (e.g., "mcp start", "mcp stop")
	if len(args) >= 1 && args[0] == "mcp" {
		if len(args) < 2 {
			return nil, fmt.Errorf("usage: pocket-advisor mcp <stdio|start|stop|status> [args] (--help for details)")
		}
		switch args[1] {
		case "stdio", "start", "stop", "status":
		default:
			return nil, fmt.Errorf("unknown mcp subcommand %q; usage: pocket-advisor mcp <stdio|start|stop|status> [args]", args[1])
		}
		o.SubCommand = "mcp " + args[1]
		// --config/-config selects the infra config file the same way it does
		// for every other mode, but it is pulled out here rather than left for
		// the subcommand's own flag.FlagSet: none of them define a config flag
		// (config selection is global, not per-subcommand), so leaving a
		// -config/--config token in SubArgs makes that FlagSet's own Parse
		// reject it as undefined and exit(2) before the subcommand ever runs.
		rest := args[2:]
		subArgs := make([]string, 0, len(rest))
		for i := 0; i < len(rest); i++ {
			arg := rest[i]
			switch {
			case arg == "-config" || arg == "--config":
				if i+1 < len(rest) {
					o.ConfigPath = rest[i+1]
					i++
				}
			case strings.HasPrefix(arg, "-config="):
				o.ConfigPath = strings.TrimPrefix(arg, "-config=")
			case strings.HasPrefix(arg, "--config="):
				o.ConfigPath = strings.TrimPrefix(arg, "--config=")
			default:
				subArgs = append(subArgs, arg)
			}
		}
		o.SubArgs = subArgs
		return o, nil
	}

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
	fs.BoolVar(&o.Doctor, "doctor", false, "read-only workspace health checks")
	fs.BoolVar(&o.Recover, "recover", false, "plan and optionally apply ingestion recovery")
	fs.BoolVar(&o.ReprocessEmail, "reprocess-email-metadata", false, "rebuild email browse metadata from Tier 1 bytes")
	fs.IntVar(&o.ReprocessLimit, "reprocess-limit", 0, "stop after this many documents (0 = whole workspace)")
	fs.IntVar(&o.ReprocessConc, "reprocess-concurrency", 0, "concurrent Tier 1 reads and metadata writes (0 = default)")
	fs.BoolVar(&o.ReprocessMissing, "reprocess-missing-only", false, "only documents with no email metadata row yet")
	fs.BoolVar(&o.Evaluate, "evaluate", false, "run retrieval quality evaluation against evaluation cases")
	fs.StringVar(&o.EvalCases, "eval-cases", "", "evaluation case set JSON (default: test/fixtures/eval/<workspace>/cases.json)")
	fs.StringVar(&o.EvalReport, "eval-report", "", "write report to path (gitignored)")
	fs.StringVar(&o.EvalFilterIDs, "eval-filter-ids", "", "comma-separated case IDs to evaluate")
	fs.StringVar(&o.EvalFilterCats, "eval-filter-cats", "", "comma-separated categories to evaluate")
	fs.BoolVar(&o.EvalRunHNSW, "eval-hnsw", false, "run exact vs HNSW dense search comparison")
	fs.IntVar(&o.EvalEfSearch, "eval-ef-search", 40, "HNSW ef_search for comparison")
	fs.BoolVar(&o.EvalReadiness, "eval-readiness", false, "check readiness without running queries")
	fs.StringVar(&o.EvalThresholds, "eval-thresholds", "", "thresholds JSON (default: built-in thresholds)")

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
		{o.Doctor, "--doctor"},
		{o.Recover, "--recover"},
		{o.ReprocessEmail, "--reprocess-email-metadata"},
		{o.Evaluate, "--evaluate"},
	} {
		if c.on {
			m = append(m, c.name)
		}
	}
	return m
}

func (o *Options) validate() error {
	// Skip validation for subcommands
	if o.SubCommand != "" {
		return nil
	}

	modes := o.modes()
	switch {
	case len(modes) == 0:
		return fmt.Errorf("no mode selected; pass one of --ingest-all, --scan, --reconcile, --listen, " +
			"--query, --doctor, --recover, --reprocess-email-metadata, --evaluate, --delete-data, " +
			"--forget, or the mcp subcommand (--help for details)")
	case len(modes) > 1:
		return fmt.Errorf("modes are mutually exclusive, got %s", strings.Join(modes, " and "))
	}

	// Every mode acts on one workspace, and a mode that silently defaulted to
	// the wrong one could purge the wrong corpus. There is no shared database
	// or bucket left to act on without one (workspace-isolation.md §13).
	if o.WorkspaceID == "" {
		return fmt.Errorf("%s requires --workspace-id", modes[0])
	}
	if o.ReprocessLimit < 0 {
		return fmt.Errorf("--reprocess-limit cannot be negative, got %d", o.ReprocessLimit)
	}
	if o.ReprocessConc < 0 {
		return fmt.Errorf("--reprocess-concurrency cannot be negative, got %d", o.ReprocessConc)
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
	if err != nil {
		return err
	}

	// Handle mcp subcommands
	if o.SubCommand != "" {
		switch o.SubCommand {
		case "mcp stdio":
			return mcpStdioCmd(cfg, o.SubArgs)
		case "mcp start":
			return mcpStartCmd(cfg, o.SubArgs)
		case "mcp stop":
			return mcpStopCmd(cfg, o.SubArgs)
		case "mcp status":
			return mcpStatusCmd(cfg, o.SubArgs)
		default:
			return fmt.Errorf("unknown subcommand: %s", o.SubCommand)
		}
	}

	// Reconcile the flag and the config onto one path, in both directions.
	// The flag previously reached only the two modes that passed it to
	// workspace.Load directly; anything resolving through cfg.Workspace
	// kept the config's own value, so --workspace-config silently did
	// nothing for most modes. That is invisible when the process runs from
	// the repository root and fatal when it does not.
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
	if o.LogDir != "" {
		cfg.LogDir = o.LogDir
	}
	if o.EmbedConc > 0 {
		cfg.Embedding.Concurrency = o.EmbedConc
	}

	// The read path logs to stderr rather than to per-role files. Those files
	// exist to separate five concurrent worker pools during an ingest; a query
	// has no pools. (The mcp subcommands never reach this line at all — they
	// return above and build their own stderr-only logs the same way, for the
	// same reason: an MCP client launches the server from a working directory
	// it may not be able to write to, and creating a relative "logs" directory
	// there fails outright — which is precisely how the first Claude Desktop
	// attempt died, before the handshake and with the reason only visible in
	// the client's own log.)
	var logs *telemetry.Logs
	if o.Query != "" {
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
	case o.Evaluate:
		eo := evaluateOptions{
			CaseSet:    o.EvalCases,
			ReportPath: o.EvalReport,
			FilterIDs:  o.EvalFilterIDs,
			FilterCats: o.EvalFilterCats,
			JSON:       o.JSON,
			RunHNSW:    o.EvalRunHNSW,
			EfSearch:   o.EvalEfSearch,
			Readiness:  o.EvalReadiness,
			Thresholds: o.EvalThresholds,
		}
		return runEvaluate(o, cfg, logs, eo)
	case o.Query != "":
		return runQuery(o, cfg, logs)
	case o.Doctor:
		return runDoctor(o, cfg, logs)
	case o.Recover:
		return runRecover(o, cfg, logs)
	case o.ReprocessEmail:
		return runReprocessEmail(o, cfg, logs)
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
