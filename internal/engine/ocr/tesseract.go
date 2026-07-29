//go:build ocr

// Tesseract via CGo. On the host this links against the Homebrew tesseract and
// leptonica runtimes; mise.toml carries the CGo include and library paths
// (ingestion-design.md §8.3).
package ocr

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"

	"github.com/otiai10/gosseract/v2"

	"github.com/suankan/pocket-advisor/internal/limits"
)

// Available reports whether this build has a real OCR engine linked.
const Available = true

// Engine holds the language set and the CPU bound OCR runs under.
//
// The bound is the process-wide CPU semaphore shared with PDF rasterisation,
// not a private limit: both burn the same cores, and two independent limits
// would oversubscribe the machine by their sum. It replaces an earlier private
// semaphore of 2, which was sized for a 1-core container rather than a host
// (§5.4).
type Engine struct {
	cpu  *limits.CPU
	lang string
}

func NewEngine(cpu *limits.CPU, lang string) *Engine {
	if lang == "" {
		// A missing language does not error — it silently produces
		// plausible-looking garbage, which is worse than a failure because it
		// reaches the index (§5.4).
		lang = "eng+rus"
	}
	return &Engine{cpu: cpu, lang: lang}
}

func (e *Engine) Close() error { return nil }

// Image runs OCR over a decoded image.
func (e *Engine) Image(ctx context.Context, img image.Image) (string, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", fmt.Errorf("encode page for ocr: %w", err)
	}
	return e.Bytes(ctx, buf.Bytes())
}

// Bytes runs OCR over encoded image bytes.
func (e *Engine) Bytes(ctx context.Context, data []byte) (string, error) {
	release, err := e.cpu.Acquire(ctx, limits.LabelOCR)
	if err != nil {
		return "", err
	}
	defer release()

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
