//go:build ocr && manual

package pdf

import (
	"context"
	"image/png"
	"os"
	"testing"

	"github.com/suankan/pocket-advisor/internal/limits"
)

// Renders page 0 through OUR engine and writes it out, so the bitmap the
// pipeline actually feeds Tesseract can be compared against pdftoppm's.
func TestRenderProbe(t *testing.T) {
	path := os.Getenv("PDF_PATH")
	if path == "" {
		t.Skip("set PDF_PATH")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := NewEngine(2, limits.NewCPU(4))
	defer e.Close()
	doc, err := e.Open(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	for _, dpi := range []int{200, 300} {
		img, cleanup, err := e.RenderPage(context.Background(), doc, 0, dpi)
		if err != nil {
			t.Fatal(err)
		}
		b := img.Bounds()
		t.Logf("dpi=%d rendered %dx%d", dpi, b.Dx(), b.Dy())
		f, _ := os.Create(os.Getenv("OUT_DIR") + "/ours-" + itoa(dpi) + ".png")
		_ = png.Encode(f, img)
		f.Close()
		cleanup()
	}
}

func itoa(i int) string {
	if i == 200 {
		return "200"
	}
	return "300"
}
