//go:build ocr && manual

package worker

import (
	"context"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/suankan/pocket-advisor/internal/engine/ocr"
	"github.com/suankan/pocket-advisor/internal/engine/pdf"
	"github.com/suankan/pocket-advisor/internal/limits"
)

// TestMissingText answers "is anything on this page absent from what we
// extracted" without anyone having to read the page and guess.
//
// Ground truth is the whole page OCR'd with no masking at all: whatever
// Tesseract can read off the bitmap is, by definition, visible on the page.
// Any word it reports that our output lacks is text a reader can see and a
// search cannot find — which is the entire failure this pass exists to close.
//
// The comparison is on lowercased word shapes rather than lines, because OCR
// and the text layer disagree about spacing and line breaks constantly and
// neither disagreement means text was lost.
//
//	PDF_PATH=... TEXT_PATH=... \
//	  mise exec -- go test -tags 'ocr manual' -run TestMissingText -v ./internal/worker/
func TestMissingText(t *testing.T) {
	path := os.Getenv("PDF_PATH")
	if path == "" {
		t.Skip("set PDF_PATH")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(os.Getenv("TEXT_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	have := words(string(got))

	cpu := limits.NewCPU(8)
	pe, err := pdf.NewEngine(2, cpu)
	if err != nil {
		t.Fatal(err)
	}
	defer pe.Close()
	ctx := context.Background()
	doc, err := pe.Open(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	eng := ocr.NewEngine(cpu, "eng")

	missing := map[string]int{}
	total := 0
	for p := 0; p < doc.PageCount(); p++ {
		img, cleanup, err := pe.RenderPage(ctx, doc, p, ResidueDPI)
		if err != nil {
			t.Fatal(err)
		}
		full, err := eng.ImageWords(ctx, img, 0)
		cleanup()
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range full {
			for tok := range words(w.Text) {
				total++
				if have[tok] > 0 {
					continue
				}
				missing[tok]++
			}
		}
	}

	keys := make([]string, 0, len(missing))
	for k := range missing {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return missing[keys[i]] > missing[keys[j]] })

	t.Logf("words visible on the page: %d", total)
	t.Logf("of those, absent from our extraction: %d distinct", len(keys))
	for i, k := range keys {
		if i >= 40 {
			t.Logf("  ... and %d more", len(keys)-i)
			break
		}
		t.Logf("  %-28s x%d", k, missing[k])
	}
}

var wordRE = regexp.MustCompile(`[\p{L}\p{N}]+`)

func words(s string) map[string]int {
	out := map[string]int{}
	for _, w := range wordRE.FindAllString(strings.ToLower(s), -1) {
		out[w]++
	}
	return out
}
