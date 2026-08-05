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
	"strings"
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
	data, err := encodeGray(img)
	if err != nil {
		return "", err
	}
	return e.Bytes(ctx, data)
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

// Word is one recognised word and where it sits in the image it came from.
type Word struct {
	Text       string
	Box        image.Rectangle
	Confidence float64
	// Line is the box of the text line this word belongs to, as Tesseract
	// grouped it. Zero when it could not be attributed to one.
	//
	// Worth carrying because Tesseract deskews the page before it decides what
	// a line is. A caller laying words out by absolute position cannot do that,
	// and on a scan even a fraction of a degree of rotation is enough for rows
	// to assemble out of two different sentences.
	Line image.Rectangle
}

// MinWordConfidence is the floor for keeping a word recovered from a page's
// residue. It is the caller's to apply, because it suits residue and nothing
// else: a page that is entirely OCR has no second source to fall back on, so
// discarding its uncertain words discards the only reading of them there is. A
// scanned licence photographed at an angle put "SYLVANIA" and half an address
// below this bar.
//
// Residue is whatever survives masking the text layer, so it is a mixture of
// genuinely missing prose and the decorative marks every letterhead carries —
// logos, rules, signatures. Tesseract's own word confidence separates them
// cleanly, and measured on real correspondence the two populations do not
// overlap: real recovered words scored 89-97 (lowest were "client's" at 90.0
// and punctuation-carrying "Bus"" at 89.0), while monogram fragments scored
// 19-77 ("tm)" 19.0, "iN" 27.5, "S®" 39.9, "=~" 60.6, "6" 76.6).
//
// 80 sits in the empty gap between them. It is word confidence rather than
// symbol confidence deliberately: Tesseract's word score folds in dictionary
// and line-fit evidence, so it answers "is this a word" rather than "is that
// shape an S" — and the junk we are rejecting is junk precisely because it is
// not word-shaped. Individual fragments can score well as shapes.
const MinWordConfidence = 80.0

// ImageWords OCRs an image and returns the words worth believing.
//
// Filtered rather than raw because the caller merges these into indexed text:
// an unfiltered residue puts "S® iN tm)" alongside real sentences, and the
// whole point of subtracting first is to keep what is added trustworthy.
func (e *Engine) ImageWords(ctx context.Context, img image.Image, minConfidence float64) ([]Word, error) {
	data, err := encodeGray(img)
	if err != nil {
		return nil, err
	}
	release, err := e.cpu.Acquire(ctx, limits.LabelOCR)
	if err != nil {
		return nil, err
	}
	defer release()

	client := gosseract.NewClient()
	defer client.Close()
	if err := client.DisableOutput(); err != nil {
		return nil, fmt.Errorf("silence tesseract diagnostics: %w", err)
	}
	if err := client.SetVariable("tessedit_pageseg_mode", "1"); err != nil {
		return nil, fmt.Errorf("set page segmentation mode: %w", err)
	}
	if err := client.SetLanguage(splitLangs(e.lang)...); err != nil {
		return nil, fmt.Errorf("set ocr language %q: %w", e.lang, err)
	}
	if err := client.SetImageFromBytes(data); err != nil {
		return nil, fmt.Errorf("load image for ocr: %w", err)
	}
	boxes, err := client.GetBoundingBoxes(gosseract.RIL_WORD)
	if err != nil {
		return nil, fmt.Errorf("ocr words: %w", err)
	}
	// Tesseract's own idea of which words form a line, which is worth having
	// because it deskews the page first. A caller laying words out by their
	// absolute position cannot: a fraction of a degree of rotation is enough
	// that a line's baseline at the left margin matches the next line's at the
	// right, and rows then assemble from two different sentences.
	lines, err := client.GetBoundingBoxes(gosseract.RIL_TEXTLINE)
	if err != nil {
		return nil, fmt.Errorf("ocr text lines: %w", err)
	}

	out := make([]Word, 0, len(boxes))
	for _, b := range boxes {
		if b.Confidence < minConfidence || strings.TrimSpace(b.Word) == "" {
			continue
		}
		w := Word{Text: b.Word, Box: b.Box, Confidence: b.Confidence}
		mid := image.Pt((b.Box.Min.X+b.Box.Max.X)/2, (b.Box.Min.Y+b.Box.Max.Y)/2)
		for _, l := range lines {
			if mid.In(l.Box) {
				w.Line = l.Box
				break
			}
		}
		out = append(out, w)
	}
	return out, nil
}

// encodeGray is Image's encoding step, shared so both paths send the client the
// same bytes.
func encodeGray(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, toGray(img)); err != nil {
		return nil, fmt.Errorf("encode page for ocr: %w", err)
	}
	return buf.Bytes(), nil
}
