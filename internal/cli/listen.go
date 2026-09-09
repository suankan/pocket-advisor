package cli

import (
	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/telemetry"
)

// Listener runs the same pipeline with startup and periodic source scans.
func runListen(o *Options, cfg *config.Config, logs *telemetry.Logs) error {
	return runIngest(o, cfg, logs)
}
