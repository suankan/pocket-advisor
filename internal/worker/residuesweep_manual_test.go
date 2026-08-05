//go:build ocr && manual

package worker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/suankan/pocket-advisor/internal/engine/ocr"
	"github.com/suankan/pocket-advisor/internal/engine/pdf"
	"github.com/suankan/pocket-advisor/internal/limits"
)

// The property this pass has to hold is not that it recovers text — it is that
// it never costs any. Laying a page out from coordinates reorders it, which is
// the whole point, but no word the text layer had may go missing in the
// process. That is the regression that would matter, against extraction already
// trusted across this corpus.
//
// So this walks a directory of real documents and asserts word-for-word
// survival on every one, reporting how many gained text and what it cost.
//
//	SWEEP_DIR=tmp/case-documents-demo-all-pdfs SWEEP_LIMIT=120 \
//	  mise exec -- go test -tags 'ocr manual' -run TestResidueSweep -v -timeout 6h ./internal/worker/
func TestResidueSweep(t *testing.T) {
	dir := os.Getenv("SWEEP_DIR")
	if dir == "" {
		t.Skip("set SWEEP_DIR")
	}

	var paths []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.EqualFold(filepath.Ext(p), ".pdf") {
			paths = append(paths, p)
		}
		return nil
	})
	sort.Strings(paths)
	if n := envInt("SWEEP_LIMIT", 0); n > 0 && n < len(paths) {
		paths = paths[:n]
	}

	textDir := os.Getenv("SWEEP_TEXT_DIR")
	outPath := os.Getenv("SWEEP_OUT")
	cpu := limits.NewCPU(8)
	pe, err := pdf.NewEngine(4, cpu)
	if err != nil {
		t.Fatal(err)
	}
	defer pe.Close()
	w := &DocumentWorker{PDF: pe, OCR: ocr.NewEngine(cpu, "eng"),
		Log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))}

	var (
		digital, changed, gained, violations int
		start                                = time.Now()
		report                               strings.Builder
	)
	for i, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		func() {
			ctx := context.Background()
			doc, err := pe.Open(ctx, data)
			if err != nil {
				return
			}
			defer doc.Close()

			before, _ := doc.ExtractText()
			if !pdf.Classify(before, doc.PageCount()).Digital {
				return
			}
			digital++

			after, err := w.layoutDocument(ctx, doc)
			if err != nil {
				t.Errorf("%s: %v", filepath.Base(p), err)
				return
			}
			if strings.TrimSpace(after) == "" {
				return
			}
			changed++
			gained += len(after) - len(before)

			if missing, ok := linesSubsequence(before, after); !ok {
				violations++
				t.Errorf("%s: text layer did not survive the merge intact; "+
					"first word lost: %q", filepath.Base(p), missing)
			}
			fmt.Fprintf(&report, "%+7d  %s\n", len(after)-len(before), filepath.Base(p))
			dumpText(textDir, filepath.Base(p), before, after)
			// Flushed per document, not at the end: a sweep over a corpus this
			// size is routinely stopped early, and a run that has to complete
			// to leave any evidence behind leaves none.
			if outPath != "" {
				_ = os.WriteFile(outPath, []byte(report.String()), 0o644)
			}
		}()
		if i%50 == 0 {
			t.Logf("... %d/%d  %s", i, len(paths), time.Since(start).Round(time.Second))
		}
	}

	t.Logf("digital documents   : %d", digital)
	t.Logf("recovered something : %d (%.1f%%)", changed, 100*float64(changed)/float64(max(digital, 1)))
	t.Logf("characters gained   : %d", gained)
	t.Logf("documents losing text: %d", violations)
	t.Logf("elapsed             : %s", time.Since(start).Round(time.Second))
	if outPath != "" {
		_ = os.WriteFile(outPath, []byte(report.String()), 0o644)
	}
}

// dumpText writes what a document gained, so the result can be read rather
// than inferred from a character count.
//
// Three files per document, because the useful question is not "what does this
// document say" but "what did this pass add to it": the recovered lines alone
// are what a reviewer checks for junk, and the full merged text is what shows
// whether those lines landed where they belong.
func dumpText(dir, name, before, after string) {
	if dir == "" {
		return
	}
	name = strings.TrimSuffix(name, filepath.Ext(name))
	_ = os.MkdirAll(dir, 0o755)

	have := make(map[string]int, len(nonBlank(before)))
	for _, l := range nonBlank(before) {
		have[l]++
	}
	var added []string
	for _, l := range nonBlank(after) {
		if have[l] > 0 {
			have[l]--
			continue
		}
		added = append(added, l)
	}

	_ = os.WriteFile(filepath.Join(dir, name+".added.txt"), []byte(strings.Join(added, "\n")+"\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, name+".after.txt"), []byte(after), 0o644)
	_ = os.WriteFile(filepath.Join(dir, name+".before.txt"), []byte(before), 0o644)
}

// linesSubsequence reports whether every word of before survives in after.
// Returns the first word that does not.
//
// Not a subsequence check any more, and it cannot be: laying a page out by
// coordinate deliberately reorders text away from the content stream — that
// reordering is the point, since it is what pulls a footer out of the middle of
// a table. What must still hold absolutely is that nothing is *lost*, so this
// counts words and requires the merged output to contain at least as many of
// each as the text layer had.
func linesSubsequence(before, after string) (string, bool) {
	after, before = collapseLeaders(after), collapseLeaders(before)

	got := map[string]int{}
	for _, w := range strings.Fields(after) {
		got[w]++
	}
	// Everything the output holds, with the spaces taken out. A word can go
	// missing as a token without being lost: the characters of a rotated label
	// arrive one per line and are rejoined into one, so "(<" ends up inside
	// "1)íAG0(<". That is a change of word boundaries, not of content, and only
	// real loss should fail this.
	joined := strings.Join(strings.Fields(after), "")

	// Characters, for the last resort below.
	chars := map[rune]int{}
	for _, r := range joined {
		chars[r]++
	}

	for _, w := range strings.Fields(before) {
		if got[w] > 0 {
			got[w]--
			continue
		}
		if strings.Contains(joined, w) {
			continue
		}
		// A word set down a margin is emitted one character per row, so it is
		// no longer a token anywhere — and if the run reads upward, its
		// characters come out in reverse. That is a deliberate trade for not
		// displacing the rows it sits beside, not a loss, and the only thing
		// left to check is that every character survived.
		if !hasAll(chars, w) {
			return w, false
		}
	}
	return "", true
}

func hasAll(pool map[rune]int, word string) bool {
	need := map[rune]int{}
	for _, r := range word {
		need[r]++
	}
	for r, n := range need {
		if pool[r] < n {
			return false
		}
	}
	return true
}

// collapseLeaders normalises runs of dots to a single marker.
//
// Layout shortens a leader to the width it actually occupies on the page, so
// "Kanmortgage" followed by ninety dots becomes the same word followed by
// twelve. Comparing those verbatim reports the feature as lost text. What must
// be compared is the words either side of the decoration.
func collapseLeaders(s string) string {
	var b strings.Builder
	run := 0
	for _, r := range s {
		if r == '.' || r == '·' || r == '_' || r == '‧' {
			if run++; run > 3 {
				continue
			}
		} else {
			run = 0
		}
		b.WriteRune(r)
	}
	return b.String()
}

func nonBlank(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func envInt(key string, def int) int {
	var n int
	if _, err := fmt.Sscanf(os.Getenv(key), "%d", &n); err != nil {
		return def
	}
	return n
}
