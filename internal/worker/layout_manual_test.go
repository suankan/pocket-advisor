//go:build ocr && manual

package worker

import (
	"context"
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

// TestLayoutPreview renders a folder of PDFs the way `pdftotext -layout` does,
// writing <basename>-layout.txt beside each source.
//
// This is a prototype of a different merge, not a variation on the current one.
// Today's output is ordered by the content stream with position used only to
// place recovered text near where it belongs; that is why a line drawn in two
// pieces comes out as two lines, and why a footer drawn early in the stream
// lands in the middle of a table. Laying out by coordinate removes both: every
// mark on the page, from the text layer or from OCR, becomes a cell with a
// position, cells that share a baseline become one row, and horizontal offsets
// become spaces.
//
// The two sources are treated identically here, which is the point — once
// everything is a positioned cell, text recovered from vector outlines occupies
// the gap in the line it was cut out of with no splicing rule at all.
func TestLayoutPreview(t *testing.T) {
	dir := os.Getenv("EXTRACT_DIR")
	if dir == "" {
		t.Skip("set EXTRACT_DIR")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".pdf") {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(paths)

	cpu := limits.NewCPU(8)
	pe, err := pdf.NewEngine(4, cpu)
	if err != nil {
		t.Fatal(err)
	}
	defer pe.Close()
	w := &DocumentWorker{PDF: pe, OCR: ocr.NewEngine(cpu, "eng"),
		Log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))}

	for _, p := range paths {
		name := filepath.Base(p)
		start := time.Now()
		data, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		func() {
			ctx := context.Background()
			doc, err := pe.Open(ctx, data)
			if err != nil {
				t.Errorf("%s: open: %v", name, err)
				return
			}
			defer doc.Close()

			var b strings.Builder
			for i := 0; i < doc.PageCount(); i++ {
				cells, err := w.pageCells(ctx, doc, i)
				if err != nil {
					t.Errorf("%s: page %d: %v", name, i, err)
					return
				}
				if i > 0 {
					b.WriteString("\n\f\n")
				}
				b.WriteString(layoutPage(cells))
			}
			out := strings.TrimSuffix(p, filepath.Ext(p)) + "-layout.txt"
			if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
				t.Errorf("%s: %v", name, err)
				return
			}
			t.Logf("%-7d chars  pages=%-4d %s  (%s)",
				b.Len(), doc.PageCount(), name, time.Since(start).Round(time.Second))
		}()
	}
}
