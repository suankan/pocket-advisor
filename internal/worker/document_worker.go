package worker

import (
	"context"
	"errors"
	"fmt"
	"image"
	"log/slog"
	"strings"
	"sync"

	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	ingestionv1 "github.com/suankan/pocket-advisor/api/proto/v1/gen"
	"github.com/suankan/pocket-advisor/internal/bus"
	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/engine/ocr"
	"github.com/suankan/pocket-advisor/internal/engine/pdf"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/storage/rustfs"
	"github.com/suankan/pocket-advisor/internal/telemetry"
	"github.com/suankan/pocket-advisor/internal/trace"
)

// RasterDPI is the rasterisation resolution for scanned pages. 300 is the
// usual floor for reliable OCR of body text.
const RasterDPI = 300

// DocumentWorker consumes both ingest.pdfs.raw and ingest.images.raw.
//
// Image OCR is folded into this pool rather than given its own, because both
// paths execute the same OCR engine against the same finite CPU budget. Two
// pools would compete for cores with no coordination (§5.4).
type DocumentWorker struct {
	Vault *rustfs.Vault
	Docs  *postgres.DocumentRepo
	Bus   *bus.Bus
	PDF   *pdf.Engine
	OCR   *ocr.Engine
	Log   *slog.Logger
}

func (w *DocumentWorker) HandlePDF(ctx context.Context, msg jetstream.Msg) error {
	var cmd ingestionv1.ProcessPdfCommand
	if err := proto.Unmarshal(msg.Data(), &cmd); err != nil {
		return Fatal(domain.ReasonMalformedCommand, err)
	}
	meta := cmd.Metadata
	if meta == nil || meta.Traceparent == "" {
		return Fatal(domain.ReasonMissingTraceContext, fmt.Errorf("command carries no traceparent"))
	}

	data, err := w.fetch(ctx, cmd.RustfsRawUri, meta.DocId)
	if err != nil {
		return err
	}
	_ = w.Docs.UpdateStatus(ctx, meta.DocId, domain.StatusProcessing, "")

	doc, err := w.PDF.Open(ctx, data)
	if err != nil {
		return WithDoc(meta.DocId, Fatal(domain.ReasonPDFOpenFailed, err))
	}
	defer doc.Close()

	// The inspection pass: extract whatever native text layer exists, then
	// decide from character density whether that layer is the document or a
	// thin index over scanned images (§5.4).
	text, _ := doc.ExtractText()
	class := pdf.Classify(text, doc.PageCount())

	if class.Digital {
		telemetry.PDFClassification.WithLabelValues("digital").Inc()
		// A text layer being dense does not make it complete, and the order it
		// extracts in is the order the file draws, not the order the page
		// reads. Lay the page out from coordinates instead, taking the text
		// layer and whatever OCR finds in the gaps it leaves (deviation 33).
		laid, err := w.layoutDocument(ctx, doc)
		if err != nil && !errors.Is(err, ocr.ErrUnavailable) {
			// Never fatal: the text layer is still the document, and losing a
			// whole letter because its logo would not OCR is the wrong trade.
			w.Log.Warn("layout failed, keeping text layer",
				"doc_id", meta.DocId, "error", err)
		} else if strings.TrimSpace(laid) != "" {
			text = laid
		}
	} else {
		telemetry.PDFClassification.WithLabelValues("scanned").Inc()
		ocrText, err := w.ocrPages(ctx, doc, meta.DocId)
		if err != nil {
			if errors.Is(err, ocr.ErrUnavailable) {
				return Decline(meta.DocId, domain.ReasonOCRUnavailable)
			}
			return WithDoc(meta.DocId, Fatal(domain.ReasonOCRFailed, err))
		}
		// Keep whichever is richer: a hybrid PDF sometimes has a usable
		// partial text layer that OCR of a low-quality scan cannot beat.
		if len(strings.TrimSpace(ocrText)) > len(strings.TrimSpace(text)) {
			text = ocrText
		}
	}

	if strings.TrimSpace(text) == "" {
		return Decline(meta.DocId, domain.ReasonEmptyExtraction)
	}
	return w.emit(ctx, meta, text, "pdf", class.PageCount)
}

// ocrPages renders a document's pages in order and OCRs them concurrently.
//
// Rendering stays serial because it has to: a Document owns one PDFium
// instance for its lifetime, and that instance is not safe for concurrent use.
// OCR carries no such constraint, so each page is handed to a goroutine as it
// comes off the renderer and the loop moves on to the next.
//
// That split is worth what it costs. Measured on a 208-page scan, rasterising
// is ~0.4s a page and OCR ~0.85s, so the old fully-sequential loop spent two
// thirds of a quarter-hour with nine of ten lanes idle. Overlapping them makes
// rendering the floor rather than a third of the total.
//
// Memory is bounded by PageSlot rather than by the loop's shape (§5.4). A slot
// is taken before rendering and released only after that page's OCR is done
// with the bitmap, so the live-bitmap ceiling is the same as when pages ran one
// at a time — the difference is that one document may now use all of it.
//
// Page order survives concurrency because results land in a slice indexed by
// page, never in completion order. A contract read out of order is not a
// contract.
func (w *DocumentWorker) ocrPages(ctx context.Context, doc *pdf.Document, docID string) (string, error) {
	if !ocr.Available {
		return "", ocr.ErrUnavailable
	}

	pages := doc.PageCount()
	texts := make([]string, pages)

	var (
		wg          sync.WaitGroup
		mu          sync.Mutex
		unavailable bool
	)

	for i := 0; i < pages; i++ {
		if ctx.Err() != nil {
			break
		}

		// Held until this page's OCR is finished with its bitmap, so the number
		// of live bitmaps never exceeds the slot count however many pages are
		// in flight.
		release, err := w.PDF.PageSlot(ctx)
		if err != nil {
			break
		}

		dpi := w.pageDPI(doc, i)
		img, cleanup, err := w.PDF.RenderPage(ctx, doc, i, dpi)
		if err != nil {
			cleanup()
			release()
			if ctx.Err() != nil {
				break
			}
			w.Log.Warn("page render failed", "doc_id", docID, "page", i, "error", err)
			continue
		}

		wg.Add(1)
		go func(page, dpi int, img *image.RGBA, cleanup, release func()) {
			defer wg.Done()
			defer release()
			defer cleanup()

			// Words, so a scan is laid out from its own geometry exactly as a
			// digital page is. A scanned statement has the same columns as a
			// born-digital one and loses them the same way if the geometry is
			// thrown away — and a licence or a form is nothing but layout.
			//
			// Safe on a scan only because each word carries the line Tesseract
			// assigned it, which is deskewed. Grouping rows by each word's own
			// baseline instead produced output 89% whitespace on a 208-page
			// scan, with adjacent lines interleaved.
			// Every word, whatever its confidence: a scan has no text layer
			// beside it, so an uncertain reading is the only reading there is.
			words, err := w.OCR.ImageWords(ctx, img, 0)
			if err != nil {
				if errors.Is(err, ocr.ErrUnavailable) {
					mu.Lock()
					unavailable = true
					mu.Unlock()
					return
				}
				w.Log.Warn("page ocr failed", "doc_id", docID, "page", page, "error", err)
				return
			}
			texts[page] = layoutPage(imageCells(words, img.Bounds().Dy(), dpi))
		}(i, dpi, img, cleanup, release)
	}

	wg.Wait()

	if unavailable {
		return "", ocr.ErrUnavailable
	}

	var b strings.Builder
	for _, t := range texts {
		if t != "" {
			fmt.Fprintf(&b, "%s\n\n", t)
		}
	}
	if ctx.Err() != nil {
		return b.String(), ctx.Err()
	}
	return b.String(), nil
}

// pageDPI renders a scan at its own resolution rather than at a fixed ceiling.
//
// Upscaling cannot recover detail a scanner never captured: rendering this
// corpus's 200 DPI contract at 300 measured 1.66x the cost for output of the
// same length. RasterDPI stays as the ceiling for pages that are genuinely
// higher resolution, and as the fallback whenever the page has no image to ask
// — a page with no image is not a page OCR was going to help anyway.
func (w *DocumentWorker) pageDPI(doc *pdf.Document, page int) int {
	native := w.PDF.NativeDPI(doc, page)
	if native <= 0 || native > RasterDPI {
		return RasterDPI
	}
	return native
}

func (w *DocumentWorker) HandleImage(ctx context.Context, msg jetstream.Msg) error {
	var cmd ingestionv1.ProcessImageCommand
	if err := proto.Unmarshal(msg.Data(), &cmd); err != nil {
		return Fatal(domain.ReasonMalformedCommand, err)
	}
	meta := cmd.Metadata
	if meta == nil || meta.Traceparent == "" {
		return Fatal(domain.ReasonMissingTraceContext, fmt.Errorf("command carries no traceparent"))
	}

	// Dimensions were recorded at discovery, so the gate can reject without a
	// fetch (§4.1).
	if ok, why := ocr.Viable(make([]byte, cmd.ByteSize), int(cmd.Width), int(cmd.Height)); !ok {
		telemetry.Skipped.WithLabelValues(why).Inc()
		return Decline(meta.DocId, domain.ReasonImageNotViable)
	}
	if !ocr.Available {
		return Decline(meta.DocId, domain.ReasonOCRUnavailable)
	}

	data, err := w.fetch(ctx, cmd.RustfsRawUri, meta.DocId)
	if err != nil {
		return err
	}
	_ = w.Docs.UpdateStatus(ctx, meta.DocId, domain.StatusProcessing, "")

	text, err := w.OCR.Bytes(ctx, data)
	if err != nil {
		if errors.Is(err, ocr.ErrUnavailable) {
			return Decline(meta.DocId, domain.ReasonOCRUnavailable)
		}
		return WithDoc(meta.DocId, Fatal(domain.ReasonOCRFailed, err))
	}

	// Post-hoc half of the viability gate: an image that OCRs to almost
	// nothing was not a document. Recorded, not retried (§5.4).
	if !ocr.EnoughText(text) {
		telemetry.Skipped.WithLabelValues("below_min_ocr_chars").Inc()
		return Decline(meta.DocId, domain.ReasonImageNotViable)
	}
	return w.emit(ctx, meta, text, "image", 1)
}

func (w *DocumentWorker) fetch(ctx context.Context, uri, docID string) ([]byte, error) {
	key, err := w.Vault.KeyFromURI(uri)
	if err != nil {
		return nil, Fatal(domain.ReasonBadObjectURI, err)
	}
	data, _, err := w.Vault.Get(ctx, key)
	if err != nil {
		return nil, WithDoc(docID, err)
	}
	return data, nil
}

func (w *DocumentWorker) emit(ctx context.Context, meta *ingestionv1.DocumentMetadata, text, docType string, pages int) error {
	if err := w.Docs.SaveText(ctx, meta.DocId, text, docType, meta.ThreadId); err != nil {
		return WithDoc(meta.DocId, err)
	}
	tp := trace.Child(meta.Traceparent)
	out := cloneMeta(meta, meta.DocId, meta.ParentDocId, meta.ThreadId, tp)
	if err := w.Bus.Publish(ctx, bus.SubjectEmbed, &ingestionv1.EmbedTextCommand{
		Metadata: out, TextLength: int64(len(text)),
	}, tp); err != nil {
		return WithDoc(meta.DocId, err)
	}
	w.Log.Info("document extracted",
		"doc_id", meta.DocId, "doc_type", docType, "pages", pages, "chars", len(text),
		"trace_id", trace.TraceID(meta.Traceparent))
	return nil
}
