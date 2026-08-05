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
//
// Orientation is Tesseract's problem, not ours: Bytes asks for page
// segmentation mode 1, which detects a page's rotation and corrects it before
// recognising anything. Nothing here needs to know which way up a scan is.
func (e *Engine) Image(ctx context.Context, img image.Image) (string, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, toGray(img)); err != nil {
		return "", fmt.Errorf("encode page for ocr: %w", err)
	}
	return e.Bytes(ctx, buf.Bytes())
}

// toGray discards colour OCR never uses.
//
// pdfium hands back RGBA whatever the source, which for the bilevel fax scans
// this mostly sees is four bytes per pixel to say black or white. Encoding grey
// halves the PNG the client has to decode and leaves the text identical —
// measured byte-for-byte on a page that exercises both.
func toGray(src image.Image) *image.Gray {
	if g, ok := src.(*image.Gray); ok {
		return g
	}
	b := src.Bounds()
	dst := image.NewGray(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			dst.Set(x, y, src.At(x, y))
		}
	}
	return dst
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

	// Send Tesseract's own diagnostics to /dev/null (it sets debug_file).
	//
	// Leptonica and Tesseract write straight to fd 2 from C, bypassing the Go
	// logger entirely, so a page they dislike floods the terminal with lines
	// like "Image too small to scale!! (2x36 vs min width of 3)" and "Scaling
	// pix of size 10, 823 by factor 0.0437 made null pix!!". A 208-page
	// bilevel fax scan emits thousands of them, which shreds the live
	// dashboard — the one surface an operator watches during a run.
	//
	// None of it is a failure: they are per-line notes about degenerate
	// regions, typically the vertical rules and signature lines of a scanned
	// contract. The page still OCRs. Losing them costs nothing we log anyway,
	// and the alternative — redirecting fd 2 process-wide — would take the Go
	// logger's own output with it.
	if err := client.DisableOutput(); err != nil {
		return "", fmt.Errorf("silence tesseract diagnostics: %w", err)
	}

	// Page segmentation mode 1: analyse the layout AND detect orientation.
	//
	// Set as a variable rather than through SetPageSegMode, which does not
	// work. That method writes to the API immediately, but Client.Text() then
	// calls Init(), and Init() resets the mode — so every value set that way is
	// discarded and Tesseract runs at its post-Init default of 6, a single
	// uniform block, with no layout or orientation analysis at all. Upstream bug,
	// github.com/otiai10/gosseract issue 167. Variables survive because gosseract
	// deliberately applies them after Init, which is the loophole this uses.
	//
	// It went unnoticed because the failure needs a page that mode 6 cannot cope
	// with. Body text on a simple page reads fine either way; a 208-page contract
	// scanned sideways came out as 400,000 characters of noise containing not one
	// instance of "vendor" or "COPYRIGHT", while the tesseract CLI — which
	// defaults to mode 3 — read the identical bitmap perfectly. That difference
	// is what exposed it.
	//
	// Mode 1 rather than 3, which is the CLI's default, because only mode 1 runs
	// orientation detection. Measured across all four quarter-turns of the same
	// page: mode 1 recovered every one, mode 3 only the two that happened to
	// suit. Vertical text in a margin survives either way, since layout analysis
	// treats it as its own block rather than rotating the whole page — a page
	// with upright body text and a sideways margin note yielded both, 33 of 34
	// words. OSD costs roughly 10% per page, which is worth it to never have to
	// ask which way up a document is.
	if err := client.SetVariable("tessedit_pageseg_mode", "1"); err != nil {
		return "", fmt.Errorf("set page segmentation mode: %w", err)
	}

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
