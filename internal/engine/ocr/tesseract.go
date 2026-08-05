//go:build ocr

// Tesseract via CGo. On the host this links against the Homebrew tesseract and
// leptonica runtimes; mise.toml carries the CGo include and library paths
// (ingestion-design.md §8.3).
package ocr

/*
#include <leptonica/allheaders.h>
*/
import "C"

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"sync"

	"github.com/otiai10/gosseract/v2"

	"github.com/suankan/pocket-advisor/internal/limits"
)

// silenceLeptonica stops the *other* C library from writing to fd 2.
//
// Tesseract and Leptonica are two libraries with two independent error paths,
// and quietening one does nothing for the other. Tesseract routes through
// tprintf() and honours the debug_file variable Bytes sets per client, which
// covers "Image too small to scale!!", "Line cannot be recognized!!" and "Bad
// pix from ImageData!". Leptonica does not: L_ERROR writes straight to stderr,
// which is where "Error in pixScaleAreaMap: pixd too small" and its dozen
// companions come from. Only setMsgSeverity reaches those.
//
// L_SEVERITY_NONE rather than something milder because the noise *is* at error
// severity — there is no threshold that keeps genuine errors and drops these,
// since to Leptonica they are the same thing. Nothing is lost: Go learns about
// failures from gosseract's return values, never from this stream, and a page
// that provokes these messages still OCRs.
//
// Process-global, so once is enough however many engines or clients exist.
var silenceLeptonica sync.Once

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
	silenceLeptonica.Do(func() { C.setMsgSeverity(C.L_SEVERITY_NONE) })

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
