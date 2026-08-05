//go:build manual

package bus

import (
	"context"
	"os"
	"testing"

	"github.com/suankan/pocket-advisor/internal/config"
)

func TestPurgeQueues(t *testing.T) {
	ws := os.Getenv("WORKSPACE_ID")
	if ws == "" {
		t.Skip("set WORKSPACE_ID")
	}
	cfg, err := config.Load(os.Getenv("CONFIG_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := cfg.Workspace(ws)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	b, err := Connect(ctx, w.NATSURL, w.NATSUser, w.NATSPassword)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	for _, n := range []string{StreamName, StreamDLQ, StreamRustFSEvents} {
		s, err := b.js.Stream(ctx, n)
		if err != nil {
			t.Logf("%-16s (absent)", n)
			continue
		}
		info, _ := s.Info(ctx)
		t.Logf("%-16s before=%d", n, info.State.Msgs)
	}
	if err := b.PurgeQueues(ctx); err != nil {
		t.Fatalf("PurgeQueues: %v", err)
	}
	for _, n := range []string{StreamName, StreamDLQ, StreamRustFSEvents} {
		s, err := b.js.Stream(ctx, n)
		if err != nil {
			continue
		}
		info, _ := s.Info(ctx)
		t.Logf("%-16s after=%d", n, info.State.Msgs)
	}
}
