//go:build manual

// Disposable measurement of exact chunk redundancy and chunk shape.
//
// It answers the cheap half of the spike without provisioning anything or
// embedding anything: chunking is a pure function of normalized_text, so the
// redundancy and chunk-shape arms can be computed by re-chunking the text
// already stored. Only if this shows a material redundancy gain is the
// expensive recall arm — which needs a disposable workspace and a full
// re-embed — worth building.
//
// Run: WORKSPACE_ID=<id> go test -tags manual ./internal/eval/ -run Spike -v
package eval

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/engine/embed"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
)

// arm is one chunking strategy's measured shape.
type arm struct {
	name     string
	chunks   int
	distinct int
	chars    int
}

func (a arm) redundant() int { return a.chunks - a.distinct }
func (a arm) pct() float64 {
	if a.chunks == 0 {
		return 0
	}
	return 100 * float64(a.redundant()) / float64(a.chunks)
}
func (a arm) meanLen() int {
	if a.chunks == 0 {
		return 0
	}
	return a.chars / a.chunks
}

// contentKeySpike matches the production identity rule: whitespace-normalised
// equality, the same comparison internal/retrieval uses to drop repeats.
func contentKeySpike(s string) string { return strings.Join(strings.Fields(s), " ") }

// zeroOverlapSplit packs text to the same budget with no overlap, snapping the
// end to a paragraph break, then a newline, then a sentence end, then a space.
// This is the "canonical chunking" arm: identical passages should produce
// identical text regardless of what precedes them.
func zeroOverlapSplit(text string) []string {
	text = strings.TrimSpace(text)
	var out []string
	for len(text) > 0 {
		if len(text) <= embed.TargetChars {
			out = append(out, text)
			break
		}
		end := snapBoundary(text, embed.TargetChars)
		piece := strings.TrimSpace(text[:end])
		if piece != "" {
			out = append(out, piece)
		}
		text = strings.TrimSpace(text[end:])
	}
	return out
}

// snapBoundary finds a split point in the final 40% of the window, preferring
// the strongest structural break available — the same preference order the
// production chunker uses for chunk ends.
func snapBoundary(text string, hardEnd int) int {
	if hardEnd > len(text) {
		hardEnd = len(text)
	}
	windowStart := int(float64(hardEnd) * 0.6)
	region := text[windowStart:hardEnd]
	if i := strings.LastIndex(region, "\n\n"); i >= 0 {
		return windowStart + i + 2
	}
	if i := strings.LastIndex(region, "\n"); i >= 0 {
		return windowStart + i + 1
	}
	if i := strings.LastIndexAny(region, ".!?"); i >= 0 {
		return windowStart + i + 1
	}
	if i := strings.LastIndex(region, " "); i >= 0 {
		return windowStart + i + 1
	}
	return hardEnd
}

// atomicSplit emits one chunk per paragraph, merging only paragraphs too small
// to stand alone and splitting any single paragraph that exceeds the budget.
// This is the maximum-deduplication arm: it never mixes two paragraphs that
// happen to be adjacent, which is what lets a shared paragraph match across
// documents regardless of its neighbours.
func atomicSplit(text string) []string {
	const minChars = 200
	var out []string
	var buf strings.Builder
	flush := func() {
		if s := strings.TrimSpace(buf.String()); s != "" {
			out = append(out, s)
		}
		buf.Reset()
	}
	for _, para := range strings.Split(text, "\n\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		if len(para) > embed.TargetChars {
			flush()
			out = append(out, zeroOverlapSplit(para)...)
			continue
		}
		if buf.Len() > 0 && buf.Len()+len(para) > embed.TargetChars {
			flush()
		}
		if buf.Len() > 0 {
			buf.WriteString("\n\n")
		}
		buf.WriteString(para)
		if buf.Len() >= minChars {
			flush()
		}
	}
	flush()
	return out
}

func TestChunkingSpike(t *testing.T) {
	ws := os.Getenv("WORKSPACE_ID")
	if ws == "" {
		t.Skip("set WORKSPACE_ID")
	}
	cfg, err := config.Load(os.Getenv("CONFIG_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	dsn, err := cfg.WorkspacePostgresDSN(ws)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db, err := postgres.Connect(ctx, dsn, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows, err := db.Pool.Query(ctx, `
SELECT normalized_text FROM documents
WHERE processing_status = 'COMPLETED' AND normalized_text IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	var docs []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		docs = append(docs, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Logf("workspace %s: %d completed documents", ws, len(docs))

	strategies := []struct {
		name string
		fn   func(string) []string
	}{
		{"current (overlap)", func(s string) []string {
			var out []string
			for _, c := range embed.Split(s) {
				out = append(out, c.Text)
			}
			return out
		}},
		{"zero-overlap packed", zeroOverlapSplit},
		{"atomic paragraphs", atomicSplit},
	}

	var arms []arm
	for _, st := range strategies {
		a := arm{name: st.name}
		seen := make(map[string]int)
		for _, d := range docs {
			for _, text := range st.fn(d) {
				a.chunks++
				a.chars += len(text)
				seen[contentKeySpike(text)]++
			}
		}
		a.distinct = len(seen)
		arms = append(arms, a)

		// Report the largest duplicate groups, which are what occupy
		// candidate slots when a query matches them.
		type g struct {
			n int
		}
		var counts []g
		for _, n := range seen {
			if n > 1 {
				counts = append(counts, g{n})
			}
		}
		sort.Slice(counts, func(i, j int) bool { return counts[i].n > counts[j].n })
		top := ""
		for i := 0; i < len(counts) && i < 5; i++ {
			top += fmt.Sprintf(" %d", counts[i].n)
		}
		t.Logf("%-22s chunks=%-6d distinct=%-6d redundant=%-5d (%4.1f%%) meanLen=%-5d groups>1=%-5d topGroups:%s",
			a.name, a.chunks, a.distinct, a.redundant(), a.pct(), a.meanLen(), len(counts), top)
	}

	base := arms[0]
	t.Log("")
	t.Log("go/no-go: redundancy must rise materially above the current figure")
	for _, a := range arms[1:] {
		t.Logf("  %-22s redundancy %4.1f%% vs %4.1f%% baseline  (delta %+.1f pts, chunks %+d)",
			a.name, a.pct(), base.pct(), a.pct()-base.pct(), a.chunks-base.chunks)
	}
}
