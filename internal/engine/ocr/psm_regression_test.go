//go:build ocr

package ocr

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/suankan/pocket-advisor/internal/engine/pdf"
	"github.com/suankan/pocket-advisor/internal/limits"
)

// The page segmentation mode has to be set as a variable, because
// SetPageSegMode is reset by Init (otiai10/gosseract#167) and Tesseract then
// runs at its post-Init default of 6 — a single uniform block, with no layout
// or orientation analysis. That failure is silent: body text on a simple page
// reads fine either way, so nothing notices until a page arrives whose layout
// mode 6 cannot cope with. This corpus went months in that state, and a
// 208-page contract came out as 400,000 characters of noise.
//
// Two markers on one page, at right angles, is the smallest thing that detects
// it. At mode 6 the vertical one is unreadable; at mode 1 both come back.
//
// The fixture is a hand-written PDF rather than a generated one: base-14
// Helvetica, no compression, 757 bytes, no tool needed to rebuild it. It does
// carry a real text layer — which is why this renders the page and OCRs the
// bitmap instead of reading the layer, or it would pass without OCR running at
// all.
func TestPageSegmentationReadsBothOrientations(t *testing.T) {
	const (
		horizontal = "HORIZONTAL BASELINE MARKER"
		vertical   = "VERTICAL MARGIN MARKER"
	)

	data, err := os.ReadFile("testdata/mixed-orientation.pdf")
	if err != nil {
		t.Fatal(err)
	}

	cpu := limits.NewCPU(2)
	pe, err := pdf.NewEngine(1, cpu)
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

	img, cleanup, err := pe.RenderPage(ctx, doc, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	text, err := NewEngine(cpu, "eng").Image(ctx, img)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.ToUpper(text)

	if !strings.Contains(got, horizontal) {
		t.Errorf("horizontal text missing — OCR is broken outright, not just on orientation.\ngot: %q", text)
	}
	if !strings.Contains(got, vertical) {
		t.Errorf("vertical text missing: the page segmentation mode is not reaching Tesseract, "+
			"so it is running at the post-Init default of 6 and doing no layout analysis. "+
			"Set tessedit_pageseg_mode as a variable, not via SetPageSegMode.\ngot: %q", text)
	}
}
