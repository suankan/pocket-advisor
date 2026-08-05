//go:build ocr && manual

package worker

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/suankan/pocket-advisor/internal/engine/ocr"
	"github.com/suankan/pocket-advisor/internal/engine/pdf"
	"github.com/suankan/pocket-advisor/internal/limits"
)

// The pipeline's own OCR engine on the pipeline's own bitmap. The tesseract CLI
// reads this page cleanly; if this does not, the fault is in our OCR path.
func TestPipelineOCROnPageZero(t *testing.T) {
	path := os.Getenv("PDF_PATH")
	if path == "" {
		t.Skip("set PDF_PATH")
	}
	data, _ := os.ReadFile(path)
	cpu := limits.NewCPU(4)
	pe, _ := pdf.NewEngine(2, cpu)
	defer pe.Close()
	doc, err := pe.Open(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	oe := ocr.NewEngine(cpu, "eng+rus")
	defer oe.Close()

	for _, dpi := range []int{200, 300} {
		img, cleanup, err := pe.RenderPage(context.Background(), doc, 0, dpi)
		if err != nil {
			t.Fatal(err)
		}
		text, err := oe.Image(context.Background(), img)
		cleanup()
		if err != nil {
			t.Fatalf("dpi %d: %v", dpi, err)
		}
		cyr := len(regexp.MustCompile(`[\x{0400}-\x{04FF}]`).FindAllString(text, -1))
		t.Logf("dpi=%d chars=%d cyrillic=%d COPYRIGHT=%v",
			dpi, len(text), cyr, strings.Contains(strings.ToUpper(text), "COPYRIGHT"))
		if len(text) > 160 {
			t.Logf("   %q", text[:160])
		}
	}
}
