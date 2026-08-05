//go:build ocr && manual

package worker

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"strings"
	"testing"

	"github.com/otiai10/gosseract/v2"

	"github.com/suankan/pocket-advisor/internal/engine/pdf"
	"github.com/suankan/pocket-advisor/internal/limits"
)

// Renders a page, masks every character the text layer already reports, OCRs
// the residue, and prints each recovered word with its confidence.
func TestResidueConfidence(t *testing.T) {
	path := os.Getenv("PDF_PATH")
	if path == "" {
		t.Skip("set PDF_PATH")
	}
	const dpi = 200
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cpu := limits.NewCPU(4)
	pe, _ := pdf.NewEngine(1, cpu)
	defer pe.Close()
	doc, err := pe.Open(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	img, cleanup, err := pe.RenderPage(context.Background(), doc, 0, dpi)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	boxes, err := doc.CharBoxes(0)
	if err != nil {
		t.Fatal(err)
	}
	masked := image.NewRGBA(img.Bounds())
	draw.Draw(masked, img.Bounds(), img, img.Bounds().Min, draw.Src)
	white := &image.Uniform{color.RGBA{255, 255, 255, 255}}
	s := float64(dpi) / 72.0
	h := float64(masked.Bounds().Dy())
	for _, b := range boxes {
		r := image.Rect(
			int(b.Left*s)-1, int(h-b.Top*s)-1,
			int(b.Right*s)+1, int(h-b.Bottom*s)+1,
		).Canon()
		draw.Draw(masked, r, white, image.Point{}, draw.Src)
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, masked)
	if out := os.Getenv("OUT_PNG"); out != "" {
		_ = os.WriteFile(out, buf.Bytes(), 0o644)
	}

	c := gosseract.NewClient()
	defer c.Close()
	_ = c.SetVariable("tessedit_pageseg_mode", "1")
	_ = c.SetLanguage("eng")
	_ = c.SetImageFromBytes(buf.Bytes())
	words, err := c.GetBoundingBoxes(gosseract.RIL_WORD)
	if err != nil {
		t.Fatal(err)
	}
	syms, err := c.GetBoundingBoxes(gosseract.RIL_SYMBOL)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("words=%d symbols=%d", len(words), len(syms))
	shown := 0
	for _, w := range words {
		if strings.TrimSpace(w.Word) == "" || shown >= 6 {
			continue
		}
		var in []gosseract.BoundingBox
		for _, s := range syms {
			if s.Box.Min.X >= w.Box.Min.X-1 && s.Box.Max.X <= w.Box.Max.X+1 &&
				s.Box.Min.Y >= w.Box.Min.Y-1 && s.Box.Max.Y <= w.Box.Max.Y+1 {
				in = append(in, s)
			}
		}
		lo, sum := 999.0, 0.0
		var parts []string
		for _, s := range in {
			if s.Confidence < lo {
				lo = s.Confidence
			}
			sum += s.Confidence
			parts = append(parts, fmt.Sprintf("%s:%.1f", s.Word, s.Confidence))
		}
		if len(in) == 0 {
			continue
		}
		t.Logf("word %-14q conf=%5.1f | min(char)=%5.1f mean(char)=%5.1f | %s",
			w.Word, w.Confidence, lo, sum/float64(len(in)), strings.Join(parts, " "))
		shown++
	}
}
