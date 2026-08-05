//go:build manual

package rustfs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/suankan/pocket-advisor/internal/config"
)

// Pulls every object listed on stdin (one "sha<TAB>key<TAB>filename" per line)
// out of a workspace's bucket and writes it to OUT_DIR.
func TestDumpObjects(t *testing.T) {
	ws, outDir, list := os.Getenv("WORKSPACE_ID"), os.Getenv("OUT_DIR"), os.Getenv("LIST")
	if ws == "" || outDir == "" || list == "" {
		t.Skip("set WORKSPACE_ID, OUT_DIR, LIST")
	}
	cfg, err := config.Load(os.Getenv("CONFIG_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := cfg.Workspace(ws)
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewForWorkspaceAt(w.RustFSEndpoint, cfg.RustFS.UseSSL, w.BucketName,
		w.RustFSAccessKey, w.RustFSSecretKey, RoleWorker)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(list)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ok, fail := 0, 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 3 {
			continue
		}
		sha, uri, name := f[0], f[1], f[2]
		key, err := v.KeyFromURI(uri)
		if err != nil {
			fail++
			continue
		}
		data, _, err := v.Get(ctx, key)
		if err != nil {
			t.Logf("  get %s: %v", name, err)
			fail++
			continue
		}
		base := strings.TrimSuffix(filepath.Base(name), ".pdf")
		if len(base) > 90 {
			base = base[:90]
		}
		out := filepath.Join(outDir, fmt.Sprintf("%s__%s.pdf", sha[:12], sanitize(base)))
		if err := os.WriteFile(out, data, 0o644); err != nil {
			t.Fatal(err)
		}
		ok++
	}
	t.Logf("written %d, failed %d", ok, fail)
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(`/\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, s)
}
