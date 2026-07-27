//go:build ocr

// Tesseract via CGo. Built only into the document-extractor image, which is
// the one binary that carries a C toolchain and the tesseract runtime
// (ingestion-design.md §8.3).
package ocr

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"sync"

	"github.com/otiai10/gosseract/v2"
)

// Available reports whether this build has a real OCR engine linked.
const Available = true

// Engine holds the concurrency bound for OCR work.
//
// A process-wide semaphore of 2, not one per document: OCR is the CPU
// bottleneck and the bitmaps are the memory hot spot, so the limit has to be
// global rather than per-request (§5.4).
type Engine struct {
	sem  chan struct{}
	lang string
	mu   sync.Mutex
}

func NewEngine(concurrency int, lang string) *Engine {
	if concurrency < 1 {
		concurrency = 2
	}
	if lang == "" {
		// A missing language does not error — it silently produces
		// plausible-looking garbage, which is worse than a failure because it
		// reaches the index (§5.4).
		lang = "eng+rus"
	}
	return &Engine{sem: make(chan struct{}, concurrency), lang: lang}
}

func (e *Engine) Close() error { return nil }

// Image runs OCR over a decoded image.
func (e *Engine) Image(img image.Image) (string, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", fmt.Errorf("encode page for ocr: %w", err)
	}
	return e.Bytes(buf.Bytes())
}

// Bytes runs OCR over encoded image bytes.
func (e *Engine) Bytes(data []byte) (string, error) {
	e.sem <- struct{}{}
	defer func() { <-e.sem }()

	client := gosseract.NewClient()
	defer client.Close()

	if err := client.SetLanguage(splitLangs(e.lang)...); err != nil {
		return "", fmt.Errorf("set ocr language %q: %w", e.lang, err)
	}
	if err := client.SetImageFromBytes(data); err != nil {
		return "", fmt.Errorf("load image for ocr: %w", err)
	}
	text, err := client.Text()
	if err != nil {
		return "", fmt.Errorf("ocr: %w", err)
	}
	return text, nil
}

func splitLangs(s string) []string {
	var out []string
	for _, p := range bytes.Split([]byte(s), []byte("+")) {
		if len(p) > 0 {
			out = append(out, string(p))
		}
	}
	return out
}
