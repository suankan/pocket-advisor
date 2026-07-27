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
	"sync"
	"time"
	"unicode"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/references"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
)

type Engine struct {
	pool pdfium.Pool
	// Rasterisation is the memory hot spot in the whole system: an A4 page at
	// 300 DPI is ~2480x3508 px, ~35MB as RGBA. Unbounded page concurrency
	// reaches a 1GiB pod limit in under 30 pages, so pages are rendered one at
	// a time and the bitmap is released before the next (§5.4).
	renderMu sync.Mutex
}

func NewEngine(maxInstances int) (*Engine, error) {
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
	return &Engine{pool: pool}, nil
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
func (d *Document) ExtractText() (string, error) {
	var b strings.Builder
	for i := 0; i < d.page; i++ {
		res, err := d.inst.GetPageText(&requests.GetPageText{
			Page: requests.Page{ByIndex: &requests.PageByIndex{Document: d.ref, Index: i}},
		})
		if err != nil {
			continue // one unreadable page must not lose the rest
		}
		if t := strings.TrimSpace(res.Text); t != "" {
			b.WriteString(t)
			b.WriteString("\n\n")
		}
	}
	return strings.TrimSpace(b.String()), nil
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
// keeps the process inside its memory limit.
func (e *Engine) RenderPage(d *Document, index, dpi int) (*image.RGBA, func(), error) {
	e.renderMu.Lock()
	defer e.renderMu.Unlock()

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
