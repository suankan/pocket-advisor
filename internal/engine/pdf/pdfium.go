// Package pdf inspects and extracts PDFs entirely in memory
// (ingestion-design.md §5.4).
//
// PDFium runs through go-pdfium's WebAssembly backend on wazero rather than
// CGo bindings. This satisfies the design's real constraints — in-process, in
// memory, no OS subprocess (Core Pillar 1) — while removing the prebuilt
// libpdfium dependency and the C-heap lifecycle risk that dominates §5.4.
// It is a documented deviation from "CGo bindings" in the design header.
package pdf

import (
	"context"
	"fmt"
	"image"
	"strings"
	"time"
	"unicode"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/references"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/responses"
	"github.com/klippa-app/go-pdfium/webassembly"

	"github.com/suankan/pocket-advisor/internal/limits"
)

type Engine struct {
	pool pdfium.Pool
	// Rasterisation is the memory hot spot in the whole system: an A4 page at
	// 300 DPI is ~2480x3508 px, ~35MB as RGBA. It is also CPU-bound, and it
	// competes for cores with OCR rather than running alongside it for free —
	// so the bound is the process-wide CPU semaphore shared with the OCR
	// engine, not a private one (§5.4).
	//
	// This deliberately replaces an earlier plain mutex here. A mutex was the
	// right shape when each worker was its own pod and the ceiling that mattered
	// was one container's 1GiB limit; in a single host process it would have
	// collapsed every replica's rasterisation into one global lane.
	cpu *limits.CPU
}

// NewEngine sizes the instance pool to the number of lanes that can hold an
// open document at once. Open() keeps its instance for the whole document, so a
// pool smaller than the lane count starves lanes on InstanceTimeout rather than
// queueing them briefly.
func NewEngine(maxInstances int, cpu *limits.CPU) (*Engine, error) {
	if maxInstances < 1 {
		maxInstances = 1
	}
	pool, err := webassembly.Init(webassembly.Config{
		MinIdle:  1,
		MaxIdle:  maxInstances,
		MaxTotal: maxInstances,
	})
	if err != nil {
		return nil, fmt.Errorf("init pdfium: %w", err)
	}
	return &Engine{pool: pool, cpu: cpu}, nil
}

func (e *Engine) Close() error { return e.pool.Close() }

// Classification is the outcome of the inspection pass.
type Classification struct {
	Digital    bool
	PageCount  int
	CharCount  int
	CharsPerPg float64
}

// Document is an open PDF plus the instance that owns it. Close releases both.
type Document struct {
	inst pdfium.Pdfium
	ref  references.FPDF_DOCUMENT
	page int
}

func (e *Engine) Open(ctx context.Context, data []byte) (*Document, error) {
	inst, err := e.pool.GetInstanceWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("pdfium instance: %w", err)
	}
	doc, err := inst.OpenDocument(&requests.OpenDocument{File: &data})
	if err != nil {
		_ = inst.Close()
		return nil, fmt.Errorf("open pdf: %w", err)
	}
	count, err := inst.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: doc.Document})
	if err != nil {
		_ = inst.Close()
		return nil, fmt.Errorf("page count: %w", err)
	}
	return &Document{inst: inst, ref: doc.Document, page: count.PageCount}, nil
}

func (d *Document) Close() {
	if d.inst != nil {
		_ = d.inst.Close()
		d.inst = nil
	}
}

func (d *Document) PageCount() int { return d.page }

// ExtractText pulls the native text layer, page by page.
//
// Uses structured char-mode extraction rather than the flat GetPageText,
// because PDFium's own line-break heuristic in GetPageText can fail on
// multi-column tabular layouts: a row's rightmost cell (e.g. a balance) and
// the next row's leftmost cell (e.g. a date) can land close enough together
// that PDFium treats them as a continuation of the same line, silently
// merging two table rows into one unreadable run.
//
// Char mode, not rect mode: rect mode's own doc comment claims it groups
// "strings on the same line with the same font settings", but measured
// directly against a dense real-world table it returned near-per-character
// rects with genuinely duplicated substrings (e.g. two overlapping rects
// both reading "in" for one "i"+"n" pair) — unreliable, not just imprecise.
// Char mode returns one verified, non-duplicated entry per character
// instead, including the odd synthetic "\r"/"\n" PDFium itself inserts at
// paragraph wraps.
//
// Those synthetic breaks alone are not enough: they fire for line-wraps
// within one text run, but never between two independent PDF text objects
// (e.g. adjacent table cells), which is exactly where the merged-row bug
// happens. Comparing each character's vertical (Top) position doesn't work
// either — measured on this same table, the real row-to-row gap was 4.8pt
// while ordinary same-line ascender/descender jitter reached 7.5pt, so no
// Y-threshold can tell them apart. What does separate them reliably is
// horizontal position: reading order only ever moves left within one line
// for minor kerning/space-glyph artifacts (measured up to ~9pt), while a
// genuine new line resets back toward the page's left margin (measured
// 180-520pt on this table) — see writeStructuredChars.
func (d *Document) ExtractText() (string, error) {
	var b strings.Builder
	for i := 0; i < d.page; i++ {
		res, err := d.inst.GetPageTextStructured(&requests.GetPageTextStructured{
			Page: requests.Page{ByIndex: &requests.PageByIndex{Document: d.ref, Index: i}},
			Mode: requests.GetPageTextStructuredModeChars,
		})
		if err != nil {
			continue // one unreadable page must not lose the rest
		}
		if writeStructuredChars(&b, res.Chars) {
			b.WriteString("\n\n")
		}
	}
	return strings.TrimSpace(b.String()), nil
}

// xBackJumpEpsilon is how far a character's left edge must fall behind the
// current line's rightmost extent so far, in PDF points, before it counts
// as a new line rather than in-line jitter. Comfortably above the ~9pt
// backward jitter a zero-width space glyph can introduce mid-line, and
// comfortably below the 180pt+ a genuine new line/row reset showed when
// measured against a real multi-column bank-statement table.
const xBackJumpEpsilon = 30.0

// writeStructuredChars concatenates one page's structured chars in order,
// inserting a line break either where PDFium's own synthetic "\n" appears
// (an in-text-run wrap) or where the next character's left edge falls well
// behind the current line's rightmost extent so far (a new PDF text object
// starting — the case PDFium's own synthetic breaks never cover, and where
// the merged-row bug lives). Reports whether it wrote anything.
func writeStructuredChars(b *strings.Builder, chars []*responses.GetPageTextStructuredChar) bool {
	wrote := false
	justBroke := true
	lineMaxRight := 0.0
	for _, c := range chars {
		switch c.Text {
		case "\r":
			continue
		case "\n":
			if !justBroke {
				b.WriteString("\n")
				justBroke = true
			}
			lineMaxRight = 0
			continue
		}
		if !justBroke && c.PointPosition.Left < lineMaxRight-xBackJumpEpsilon {
			b.WriteString("\n")
			lineMaxRight = 0
			justBroke = true
		}
		b.WriteString(c.Text)
		if strings.TrimSpace(c.Text) != "" {
			justBroke = false
			wrote = true
		}
		if c.PointPosition.Right > lineMaxRight {
			lineMaxRight = c.PointPosition.Right
		}
	}
	return wrote
}

// Classify decides digital vs scanned from character density.
//
// The threshold is deliberately low: a hybrid PDF with a thin text layer over
// scanned images should go to OCR, because the text layer is usually a partial
// index rather than the document's content.
const minCharsPerPage = 100

func Classify(text string, pages int) Classification {
	printable := 0
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			printable++
		}
	}
	perPage := 0.0
	if pages > 0 {
		perPage = float64(printable) / float64(pages)
	}
	return Classification{
		Digital:    perPage >= minCharsPerPage,
		PageCount:  pages,
		CharCount:  printable,
		CharsPerPg: perPage,
	}
}

// RenderPage rasterises one page at the given DPI. The caller must call the
// returned cleanup before rendering the next page — that discipline is what
// bounds the number of live bitmaps to the number of CPU slots.
func (e *Engine) RenderPage(ctx context.Context, d *Document, index, dpi int) (*image.RGBA, func(), error) {
	release, err := e.cpu.Acquire(ctx, limits.LabelRasterize)
	if err != nil {
		return nil, func() {}, err
	}
	defer release()

	res, err := d.inst.RenderPageInDPI(&requests.RenderPageInDPI{
		Page: requests.Page{ByIndex: &requests.PageByIndex{Document: d.ref, Index: index}},
		DPI:  dpi,
	})
	if err != nil {
		return nil, func() {}, fmt.Errorf("render page %d: %w", index, err)
	}
	cleanup := res.Cleanup
	if cleanup == nil {
		cleanup = func() {}
	}
	return res.Result.Image, cleanup, nil
}

// InstanceTimeout bounds how long a worker waits for a pdfium instance.
const InstanceTimeout = 30 * time.Second
