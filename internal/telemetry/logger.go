// Package telemetry wires structured logging and Prometheus metrics.
// Log records are JSON so they parse without a regex (ingestion-design.md §9.4).
package telemetry

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Role names. One log file per role, because a monolith interleaves six roles
// that used to have a pod each — and `kubectl logs -l app=email-processor` was
// how you read one of them in isolation. The files are that, restored.
const (
	RoleApp      = "pocket-advisor"
	RoleUploader = "uploader"
	RoleDiscover = "discovery"
	RoleEmail    = "email-processor"
	RoleDocument = "document-extractor"
	RoleOffice   = "office-extractor"
	RoleEmbed    = "embed-indexer"
)

// Roles is every role that gets a log file, in pipeline order.
var Roles = []string{
	RoleApp, RoleUploader, RoleDiscover,
	RoleEmail, RoleDocument, RoleOffice, RoleEmbed,
}

// Logs owns the per-role log files for a run.
//
// The terminal belongs to the dashboard while a run is live, so these files are
// the only complete record of what happened. Nothing here writes to stdout.
type Logs struct {
	dir   string
	level slog.Level

	stderr  bool
	mu      sync.Mutex
	files   []io.Closer
	loggers map[string]*slog.Logger
}

// StderrLogs sends every role to stderr and touches no files.
//
// For the read path this is the right destination, not a fallback. --query is
// a one-shot command whose operator is watching the terminal, and the mcp
// stdio/start subcommands are launched by a client that captures the
// server's stderr into its own log — which is exactly where someone
// debugging a failed server will look. Both may also be started from a
// directory they cannot write to, which is fatal for the file-backed logger
// and meaningless for them.
func StderrLogs(level string) *Logs {
	return &Logs{
		dir:     "",
		stderr:  true,
		level:   parseLevel(level),
		loggers: make(map[string]*slog.Logger),
	}
}

// OpenLogs prepares the log directory. Files are opened lazily, one per role
// asked for, and appended to rather than truncated so a resumed run does not
// erase the record of the run it is resuming.
func OpenLogs(dir, level string) (*Logs, error) {
	if dir == "" {
		dir = "logs"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create log dir %s: %w", dir, err)
	}
	return &Logs{
		dir:     dir,
		level:   parseLevel(level),
		loggers: make(map[string]*slog.Logger),
	}, nil
}

// Dir reports where the logs are being written, for the closing summary.
func (l *Logs) Dir() string { return l.dir }

// Logger returns the logger for one role, creating its file on first use.
//
// A role whose file cannot be opened still gets a working logger pointed at
// stderr: losing a log file is not a reason to fail an ingest run.
func (l *Logs) Logger(role string) *slog.Logger {
	l.mu.Lock()
	defer l.mu.Unlock()

	if lg, ok := l.loggers[role]; ok {
		return lg
	}

	if l.stderr {
		lg := slog.New(slog.NewJSONHandler(os.Stderr,
			&slog.HandlerOptions{Level: l.level})).With("worker_type", role)
		l.loggers[role] = lg
		return lg
	}

	var w io.Writer
	f, err := os.OpenFile(
		filepath.Join(l.dir, role+".log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: log file for %s unavailable (%v); logging to stderr\n", role, err)
		w = os.Stderr
	} else {
		w = f
		l.files = append(l.files, f)
	}

	lg := slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: l.level})).
		With("worker_type", role)
	l.loggers[role] = lg
	return lg
}

// Paths lists the log files opened so far.
func (l *Logs) Paths() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	out := make([]string, 0, len(l.loggers))
	for role := range l.loggers {
		out = append(out, filepath.Join(l.dir, role+".log"))
	}
	return out
}

func (l *Logs) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	var firstErr error
	for _, f := range l.files {
		if err := f.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	l.files = nil
	return firstErr
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// NewLogger returns a stderr logger for the paths that run before, or instead
// of, the per-role files: flag validation, config errors, and any failure that
// has to be visible when the dashboard never starts.
func NewLogger(role, level string) *slog.Logger {
	h := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: parseLevel(level)})
	return slog.New(h).With("worker_type", role)
}
