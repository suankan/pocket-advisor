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
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/domain"
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
	Reconcile  bool
	Listen     bool
	DeleteData bool
	Forget     string
	Query      string

	// ReconcileDeletions is the sweep for documents whose originals are gone.
	// Ingestion is additive; this previews the sweep and applies it only with
	// --yes and a typed confirmation.
	ReconcileDeletions bool

	TopK        int
	JSON        bool
	NoRerank    bool
	NoDecompose bool
	DryRun      bool
	Yes         bool
	OCRLangs    string
	EmbedConc   int
	LogDir      string

	Doctor  bool
	Recover bool
	// RecoverReasons narrows --recover to FAILED documents with these
	// reasons and reruns them whatever their class: naming a reason is the
	// operator's judgment that an unclassified or terminal one waits for.
	// RecoverAll, from --reason all, reruns every FAILED document alongside
	// the stale work the default selection takes.
	RecoverReasons []domain.FailureReason
	RecoverAll     bool
	reasonArg      string

	// Golden-case authoring (retrieval-design.md's evaluation section).
	// CorpusSearch deliberately bypasses retrieval: a case whose expected
	// answer came from the ranker can never fail for the ranker missing a
	// document, which is the failure worth catching.
	CorpusSearch string

	Evaluate       bool
	EvalCases      string
	EvalReport     string
	EvalFilterIDs  string
	EvalRunHNSW    bool
	EvalEfSearch   int
	EvalReadiness  bool
	EvalThresholds string

	StaleAfter time.Duration
}

const usage = `pocket-advisor — ingestion pipeline for a local RAG corpus.

Modes (exactly one):
  --ingest-all        scan configured originals and process queued jobs; never
                      deletes (see --reconcile-deletions)
  --reconcile         scan configured originals, retry jobs waiting for a source
                      that has returned, and process unfinished durable jobs
  --listen            scan at startup and every minute while processing jobs
  --query <question>  ask the corpus a question and print the matching
                      sources, with citations. Returns evidence, not an
                      answer: generation happens outside this binary
                      (retrieval-design.md §6.1)
  --corpus-search <text>
                      find every document whose text contains that exact
                      wording, searching the corpus directly rather than
                      through retrieval. Whitespace runs are collapsed
                      before comparing, so extracted PDF layout does not
                      hide a match. Use it to author a golden case: the
                      answer has to be established independently of the
                      ranker, or the case can never fail for the ranker
                      missing a document. A full scan, tens of seconds.
  --doctor            read-only workspace health checks
  --recover           plan and optionally apply ingestion recovery
                      (default: dry-run; use --yes to apply)
                      --reason <R1,R2,...> plans only FAILED documents with
                      those reasons, and reruns them whatever their class;
                      --reason all plans every FAILED document and the
                      stale work plain --recover takes
  --evaluate          run retrieval quality evaluation against evaluation cases.
                      Uses workspaces/evaluation/<workspace>/cases.json by default.
                      --json emits machine-readable output.
  --delete-data       clear derived database state and the job queue; preserve
                      original files
  --forget <sha256>   remove one document by content hash
  --reconcile-deletions
                      sweep away documents nothing stages any more; ingestion
                      never does this itself. That covers a file deleted from
                      the workspace, a file an ignore: pattern now excludes,
                      and anything left held by nothing else once those go.
                      Compares content, not filenames, and refuses if the
                      directory is empty. Reports the plan and deletes nothing
                      unless --yes is supplied and the deletion is confirmed.
                      May prepare derived containment metadata before
                      reporting the plan.

MCP (a subcommand, not a mode flag):
  mcp stdio --workspace-id <id>
                      serve the read path as an MCP tool over stdio, so an
                      agent can search the corpus and write cited answers.
                      This is where answer generation happens — outside this
                      binary (retrieval-design.md §6.1)
  mcp start --workspace-id <id> [options]
                      start the same fixed-workspace tools over Streamable
                      HTTP as a detached background daemon on loopback,
                      and return once it is confirmed running.
                      Plain, unauthenticated HTTP; TLS and
                      authentication belong to a gateway. See docs/mcp.md.
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

Evaluate options:
  --eval-cases <path>        evaluation case set JSON (default: workspaces/evaluation/<workspace>/cases.json)
  --eval-report <path>       write the report somewhere other than the
                             default workspaces/evaluation/<workspace>/runs/
  --eval-filter-ids <csv>    comma-separated case IDs to evaluate
  --eval-hnsw                run exact vs HNSW dense search comparison
  --eval-ef-search N         HNSW ef_search for comparison (default 40)
  --eval-readiness           check readiness without running queries
  --eval-thresholds <path>   thresholds JSON (default: built-in thresholds)

Options:
  --yes                     skip destructive confirmation prompts
  --dry-run                 with --ingest-all: report file and byte counts, write nothing
  --embedding-concurrency N concurrent embedding sessions (default 8)
  --ocr-langs <langs>       tesseract languages (default eng+rus)
  --log-dir <path>          per-role log files (default logs)

Every other pool is sized from the host's CPU count and is not configurable.

Examples:
  pocket-advisor --ingest-all --workspace-id test
  pocket-advisor --query "what did we agree about the school holidays?" --workspace-id test
  pocket-advisor mcp stdio --workspace-id test
  pocket-advisor mcp start --workspace-id test
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

	fs.BoolVar(&o.IngestAll, "ingest-all", false, "scan originals, enqueue and process to completion")
	fs.BoolVar(&o.Reconcile, "reconcile", false, "scan originals, retry source waits and process unfinished jobs")
	fs.BoolVar(&o.Listen, "listen", false, "scan originals every minute while running the pipeline")
	fs.BoolVar(&o.DeleteData, "delete-data", false, "clear derived database state and the job queue; preserve original files")
	fs.StringVar(&o.Forget, "forget", "", "remove one document by sha256")
	fs.BoolVar(&o.ReconcileDeletions, "reconcile-deletions", false,
		"remove documents whose staged file is gone (plans unless --yes)")
	fs.StringVar(&o.Query, "query", "", "ask the corpus a question and print the matching sources")
	fs.StringVar(&o.CorpusSearch, "corpus-search", "",
		"find documents containing this exact text, bypassing retrieval")
	fs.BoolVar(&o.Doctor, "doctor", false, "read-only workspace health checks")
	fs.BoolVar(&o.Recover, "recover", false, "plan and optionally apply ingestion recovery")
	fs.StringVar(&o.reasonArg, "reason", "", "with --recover: rerun only FAILED documents with these comma-separated reasons, whatever their class; \"all\" reruns every FAILED and stale document")
	fs.BoolVar(&o.Evaluate, "evaluate", false, "run retrieval quality evaluation against evaluation cases")
	fs.StringVar(&o.EvalCases, "eval-cases", "", "evaluation case set JSON (default: test/fixtures/eval/<workspace>/cases.json)")
	fs.StringVar(&o.EvalReport, "eval-report", "", "write the report somewhere other than the per-workspace runs directory")
	fs.StringVar(&o.EvalFilterIDs, "eval-filter-ids", "", "comma-separated case IDs to evaluate")
	fs.BoolVar(&o.EvalRunHNSW, "eval-hnsw", false, "run exact vs HNSW dense search comparison")
	fs.IntVar(&o.EvalEfSearch, "eval-ef-search", 40, "HNSW ef_search for comparison")
	fs.BoolVar(&o.EvalReadiness, "eval-readiness", false, "check readiness without running queries")
	fs.StringVar(&o.EvalThresholds, "eval-thresholds", "", "thresholds JSON (default: built-in thresholds)")

	fs.IntVar(&o.TopK, "top-k", 0, "maximum results for --query (0 = config default)")
	fs.BoolVar(&o.JSON, "json", false, "emit --query results as JSON")
	fs.BoolVar(&o.NoRerank, "no-rerank", false, "skip reranking; serve fused order (--query)")
	fs.BoolVar(&o.NoDecompose, "no-decompose", false, "do not split the question into sub-queries (--query)")

	fs.BoolVar(&o.DryRun, "dry-run", false, "report what would be admitted, write nothing")
	fs.BoolVar(&o.Yes, "yes", false, "skip destructive confirmation prompts")
	fs.StringVar(&o.OCRLangs, "ocr-langs", "eng+rus", "tesseract languages")
	fs.IntVar(&o.EmbedConc, "embedding-concurrency", 0, "concurrent embedding sessions (0 = config default)")
	fs.StringVar(&o.LogDir, "log-dir", "", "per-role log directory (empty = config default)")

	fs.DurationVar(&o.StaleAfter, "stale-after", 30*time.Minute, "PENDING age that counts as stalled")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	// The flag package stops at the first bare word and leaves every flag
	// after it unread, so "--recover all --yes" would silently drop --yes.
	// No mode takes a bare word, so one is an error rather than a cut-off.
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q: options are --flags, and none after it was read (--help for details)", fs.Arg(0))
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
		{o.Reconcile, "--reconcile"},
		{o.Listen, "--listen"},
		{o.DeleteData, "--delete-data"},
		{o.Forget != "", "--forget"},
		{o.ReconcileDeletions, "--reconcile-deletions"},
		{o.Query != "", "--query"},
		{o.Doctor, "--doctor"},
		{o.Recover, "--recover"},
		{o.Evaluate, "--evaluate"},
		{o.CorpusSearch != "", "--corpus-search"},
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
		return fmt.Errorf("no mode selected; pass one of --ingest-all, --reconcile, --listen, " +
			"--query, --doctor, --recover, --evaluate, --corpus-search, --delete-data, " +
			"--forget, or the mcp subcommand (--help for details)")
	case len(modes) > 1:
		return fmt.Errorf("modes are mutually exclusive, got %s", strings.Join(modes, " and "))
	}

	// Every mode acts on one workspace, and a mode that silently defaulted to
	// the wrong one could purge the wrong corpus. There is no shared database
	// left to act on without one (workspace-isolation.md §13).
	if o.WorkspaceID == "" {
		return fmt.Errorf("%s requires --workspace-id", modes[0])
	}
	if o.DryRun && !o.IngestAll {
		return fmt.Errorf("--dry-run requires --ingest-all")
	}
	if o.reasonArg != "" {
		if !o.Recover {
			return fmt.Errorf("--reason requires --recover")
		}
		if err := o.parseReasons(); err != nil {
			return err
		}
	}
	if o.Forget != "" && len(o.Forget) != 64 {
		return fmt.Errorf("--forget expects a 64-character sha256, got %d characters", len(o.Forget))
	}
	return nil
}

// parseReasons reads --reason: "all" on its own, or comma-separated reasons a
// document can fail with, case-insensitive and each named once.
func (o *Options) parseReasons() error {
	for _, part := range strings.Split(o.reasonArg, ",") {
		r := domain.FailureReason(strings.ToUpper(strings.TrimSpace(part)))
		switch {
		case r == "ALL":
			o.RecoverAll = true
		case slices.Contains(domain.ProcessingFailureReasons(), r) || slices.Contains(domain.DeclineReasons(), r):
			if !slices.Contains(o.RecoverReasons, r) {
				o.RecoverReasons = append(o.RecoverReasons, r)
			}
		default:
			return fmt.Errorf("--reason %q is not a reason a document can fail with", strings.TrimSpace(part))
		}
	}
	if o.RecoverAll && len(o.RecoverReasons) > 0 {
		return fmt.Errorf("--reason all already includes every reason; name it alone")
	}
	return nil
}

// NeedsPipeline reports whether this mode enqueues work and must therefore also
// drain it. In a monolith an enqueueing mode that did not run the pools would
// publish into a void: nothing else is listening.
func (o *Options) NeedsPipeline() bool {
	return o.IngestAll || o.Reconcile || o.Listen
}

// Mode is the selected mode's name.
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
			return mcpStartCmd(cfg, o.ConfigPath, o.SubArgs)
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
	// return above and build their own file-backed RoleMCP logs the same way
	// every other long-running role does; see telemetry.StderrLogs.)
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
	case o.ReconcileDeletions:
		return runReconcileDeletions(o, cfg, logs)
	case o.Evaluate:
		eo := evaluateOptions{
			CaseSet:    o.EvalCases,
			ReportPath: o.EvalReport,
			FilterIDs:  o.EvalFilterIDs,
			JSON:       o.JSON,
			RunHNSW:    o.EvalRunHNSW,
			EfSearch:   o.EvalEfSearch,
			Readiness:  o.EvalReadiness,
			Thresholds: o.EvalThresholds,
		}
		return runEvaluate(o, cfg, logs, eo)
	case o.Query != "":
		return runQuery(o, cfg, logs)
	case o.CorpusSearch != "":
		return runCorpusSearch(o, cfg, logs, o.CorpusSearch)
	case o.Doctor:
		return runDoctor(o, cfg, logs)
	case o.Recover:
		return runRecover(o, cfg, logs)
	case o.Listen:
		return runListen(o, cfg, logs)
	default:
		return runIngest(o, cfg, logs)
	}
}

// interrupts wires the two-stage stop.
//
// The first signal stops claiming but lets in-flight handlers finish and
// complete. The second signal, or a drain that overruns, cancels the work
// itself; each cancelled job is returned to the queue with its attempt
// refunded, so interrupted runs never spend a document's retry budget.
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
// 5-minute job lease, so a handler still running when it elapses is cancelled
// while it still owns its job and can hand it back cleanly.
const drainGrace = 2 * time.Minute

// confirm gates a destructive action on stdin. A variable so tests can answer.
var confirm = func(prompt string) bool {
	fmt.Printf("%s\nType 'yes' to continue: ", prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	return strings.TrimSpace(strings.ToLower(line)) == "yes"
}

// errAborted is returned when the operator declines a confirmation.
var errAborted = errors.New("aborted")
