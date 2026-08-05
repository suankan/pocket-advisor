//go:build ocr && manual

package pdf

import (
	"context"
	"os"
	"regexp"
	"testing"

	"github.com/suankan/pocket-advisor/internal/limits"
)

// What does the inspection pass actually see for a scan with no fonts?
func TestExtractTextAndClassify(t *testing.T) {
	path := os.Getenv("PDF_PATH")
	if path == "" {
		t.Skip("set PDF_PATH")
	}
	data, _ := os.ReadFile(path)
	e, _ := NewEngine(2, limits.NewCPU(4))
	defer e.Close()
	doc, err := e.Open(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	text, err := doc.ExtractText()
	if err != nil {
		t.Logf("ExtractText error: %v", err)
	}
	cls := Classify(text, doc.PageCount())
	t.Logf("native text layer: %d chars over %d pages", len(text), doc.PageCount())
	t.Logf("Classify -> Digital=%v CharCount=%d CharsPerPg=%.1f", cls.Digital, cls.CharCount, cls.CharsPerPg)
	cyr := len(regexp.MustCompile(`[\x{0400}-\x{04FF}]`).FindAllString(text, -1))
	t.Logf("cyrillic chars in native layer: %d", cyr)
	if len(text) > 200 {
		t.Logf("first 200: %q", text[:200])
	}
}
