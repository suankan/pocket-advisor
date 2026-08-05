//go:build ocr && manual

package pdf

import (
	"context"
	"os"
	"testing"

	"github.com/suankan/pocket-advisor/internal/limits"
)

// Manual: needs a real scanned PDF, so it is behind a build tag rather than in
// the default suite. Run with:
//
//	go test -tags 'ocr manual' -run NativeDPI -v ./internal/engine/pdf/ \
//	  -pdf "tmp/large-pdf/Contract for land exchanged.pdf"
var pdfPath = os.Getenv("PDF_PATH")

func TestNativeDPIOnARealScan(t *testing.T) {
	if pdfPath == "" {
		t.Skip("set PDF_PATH")
	}
	data, err := os.ReadFile(pdfPath)
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
	t.Logf("pages: %d", doc.PageCount())
	for _, p := range []int{0, 1, 50, doc.PageCount() - 1} {
		t.Logf("  page %3d native DPI = %d", p, e.NativeDPI(doc, p))
	}
}
