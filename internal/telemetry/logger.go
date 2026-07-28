// Package telemetry wires structured logging and Prometheus metrics.
// Log records are JSON on stdout so VLAgent parses them without a regex
// (ingestion-design.md §9.4).
package telemetry

import (
	"log/slog"
	"os"
	"strings"
)

// NewLogger returns the process logger. worker is emitted on every record as
// worker_type, one of the mandatory fields in §9.4.
func NewLogger(worker, level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(h).With("worker_type", worker)
}
