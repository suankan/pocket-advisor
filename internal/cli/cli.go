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

	IngestAll       bool
	Scan            bool
	Reconcile       bool
	DeleteData      bool
	Forget          string
	Bootstrap       bool
	CreateWorkspace bool
	DeleteWorkspace bool

	DryRun      bool
	Yes         bool
	OCRLangs    string
	EmbedConc   int
	LogDir      string
	NoDashboard bool

	StaleAfter time.Duration
	HighWater  uint64
	LowWater   uint64
}

const usage = `pocket-advisor — ingestion pipeline for a local RAG corpus.

Modes (exactly one):
  --ingest-all        upload the workspace, enqueue what is new, process to completion
  --scan              enqueue Tier 1 objects with no Tier 2 row, then process
  --reconcile         re-publish documents stuck PENDING, then process
  --delete-data       purge the workspace from Tier 1 and Tier 2
  --forget <sha256>   remove one document by content hash
  --bootstrap-schema  probe the embedding endpoint and (re-)apply this
                      workspace's DDL — --create-workspace already does
                      this once; use this to re-probe after a model change
  --create-workspace  provision this workspace's Postgres DB+role, RustFS
                      bucket+identity, and NATS account+user
  --delete-workspace  tear down the same three, in reverse order

Common:
  --workspace-id <id>       workspace from the registry (required by most modes)
  --workspace-config <path> registry path (default workspaces/workspace-config.yaml)
  --config <path>           infrastructure config (default config.yaml)

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
  pocket-advisor --delete-data --workspace-id test
`

// Parse reads the command line into Options.
func Parse(args []string) (*Options, error) {
	o := &Options{}
	fs := flag.NewFlagSet("pocket-advisor", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }

	fs.StringVar(&o.ConfigPath, "config", config.DefaultPath, "infrastructure config path")
	fs.StringVar(&o.WorkspaceConfig, "workspace-config", "workspaces/workspace-config.yaml", "workspace registry path")
	fs.StringVar(&o.WorkspaceID, "workspace-id", "", "workspace id within the registry")

	fs.BoolVar(&o.IngestAll, "ingest-all", false, "upload, enqueue and process to completion")
	fs.BoolVar(&o.Scan, "scan", false, "enqueue un-stubbed Tier 1 objects, then process")
	fs.BoolVar(&o.Reconcile, "reconcile", false, "re-publish stalled PENDING documents, then process")
	fs.BoolVar(&o.DeleteData, "delete-data", false, "purge the workspace from Tier 1 and Tier 2")
	fs.StringVar(&o.Forget, "forget", "", "remove one document by sha256")
	fs.BoolVar(&o.Bootstrap, "bootstrap-schema", false, "probe the embedding endpoint and apply the DDL")
	fs.BoolVar(&o.CreateWorkspace, "create-workspace", false, "provision this workspace's database, bucket, and NATS account")
	fs.BoolVar(&o.DeleteWorkspace, "delete-workspace", false, "tear down this workspace's database, bucket, and NATS account")

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
		{o.DeleteData, "--delete-data"},
		{o.Forget != "", "--forget"},
		{o.Bootstrap, "--bootstrap-schema"},
		{o.CreateWorkspace, "--create-workspace"},
		{o.DeleteWorkspace, "--delete-workspace"},
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
		return fmt.Errorf("no mode selected; pass one of --ingest-all, --scan, --reconcile, " +
			"--delete-data, --forget, --bootstrap-schema, --create-workspace, --delete-workspace " +
			"(--help for details)")
	case len(modes) > 1:
		return fmt.Errorf("modes are mutually exclusive, got %s", strings.Join(modes, " and "))
	}

	// Every mode acts on one workspace, and a mode that silently defaulted to
	// the wrong one could purge the wrong corpus. --bootstrap-schema is no
	// exception: there is no shared database left to bootstrap without one
	// (workspace-isolation.md §13) — it applies (or re-applies) exactly one
	// workspace's schema, the same as --create-workspace's own schema step.
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
	return o.IngestAll || o.Scan || o.Reconcile
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
	if o.LogDir != "" {
		cfg.LogDir = o.LogDir
	}
	if o.EmbedConc > 0 {
		cfg.Embedding.Concurrency = o.EmbedConc
	}

	logs, err := telemetry.OpenLogs(cfg.LogDir, cfg.LogLevel)
	if err != nil {
		return err
	}
	defer logs.Close()

	switch {
	case o.Bootstrap:
		return runBootstrap(o, cfg, logs)
	case o.DeleteData, o.Forget != "":
		return runReset(o, cfg, logs)
	case o.CreateWorkspace:
		return runCreateWorkspace(o, cfg, logs)
	case o.DeleteWorkspace:
		return runDeleteWorkspace(o, cfg, logs)
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
