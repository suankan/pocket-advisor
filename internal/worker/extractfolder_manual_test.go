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

// TestExtractFolder writes what ingestion would store for every PDF in a
// directory, as <basename>.txt beside the source.
//
// It follows HandlePDF's branching rather than calling one extractor, because
// the interesting question about a folder of real documents is what the
// pipeline does with them, and that depends on how each one classifies:
// digital pages get the text layer plus whatever subtract-and-merge recovers
// from the gaps, scanned pages get OCR and keep whichever of the two is richer.
// Reproducing that here is what makes the output comparable to production.
//
// Each file is written as it finishes, so a run stopped partway leaves every
// document it had already done.
//
//	EXTRACT_DIR=$PWD/tmp/pdf-to-text-experiments \
//	  mise exec -- go test -tags 'ocr manual' -run TestExtractFolder -v -timeout 3h ./internal/worker/
func TestExtractFolder(t *testing.T) {
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
	if len(paths) == 0 {
		t.Fatalf("no PDFs in %s", dir)
	}

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

			layer, _ := doc.ExtractText()
			class := pdf.Classify(layer, doc.PageCount())

			text, kind := layer, "digital"
			var added []string
			if class.Digital {
				// The page's own layout, from the text layer plus whatever OCR
				// finds in the gaps it leaves.
				var b strings.Builder
				for i := 0; i < doc.PageCount(); i++ {
					cells, err := w.pageCells(ctx, doc, i)
					if err != nil {
						t.Errorf("%s: page %d: %v", name, i, err)
						return
					}
					for _, c := range cells {
						if c.ocr {
							added = append(added, c.text)
						}
					}
					page := layoutPage(cells)
					if strings.TrimSpace(page) == "" {
						continue
					}
					if b.Len() > 0 {
						b.WriteString("\n\n")
					}
					b.WriteString(page)
				}
				text = trimBlankLines(b.String())
			} else {
				kind = "scanned"
				ocrText, err := w.ocrPages(ctx, doc, name)
				if err != nil {
					t.Errorf("%s: ocr: %v", name, err)
					return
				}
				if len(strings.TrimSpace(ocrText)) > len(strings.TrimSpace(layer)) {
					text = ocrText
				}
				// A scanned page has no text layer to add to: all of it is OCR.
				added = strings.Split(strings.TrimSpace(ocrText), "\n")
			}

			base := strings.TrimSuffix(p, filepath.Ext(p))
			for suffix, body := range map[string]string{
				"-text-extracted.txt": layer,
				"-ocr-added.txt":      strings.Join(added, "\n"),
				"-result.txt":         text,
			} {
				if err := os.WriteFile(base+suffix, []byte(body), 0o644); err != nil {
					t.Errorf("%s: write%s: %v", name, suffix, err)
					return
				}
			}
			t.Logf("%-9s pages=%-4d layer=%-7d final=%-7d gained=%+-7d lines=%-4d %s  (%s)",
				kind, doc.PageCount(), len(layer), len(text), len(text)-len(layer),
				len(added), name, time.Since(start).Round(time.Second))
		}()
	}
}
