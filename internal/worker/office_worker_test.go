package worker

import (
	"log/slog"
	"testing"
)

// Text an extractor carries over from a binary format loses its NULs, which
// PostgreSQL cannot store and a reader never sees.
func TestWithoutNULRemovesOnlyNUL(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	if got := withoutNUL(log, "Why\x00 people\x00 move", "doc"); got != "Why people move" {
		t.Errorf("got %q", got)
	}
	if got := withoutNUL(log, "plain text", "doc"); got != "plain text" {
		t.Errorf("got %q", got)
	}
}
