package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

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
		return Fatal("MALFORMED_COMMAND", err)
	}
	meta := cmd.Metadata
	if meta == nil || meta.Traceparent == "" {
		return Fatal("MISSING_TRACE_CONTEXT", fmt.Errorf("command carries no traceparent"))
	}

	data, err := w.fetch(ctx, cmd.RustfsRawUri, meta.DocId)
	if err != nil {
		return err
	}
	_ = w.Docs.UpdateStatus(ctx, meta.DocId, domain.StatusProcessing, "")

	doc, err := w.PDF.Open(ctx, data)
	if err != nil {
		return WithDoc(meta.DocId, Fatal("PDF_OPEN_FAILED", err))
	}
	defer doc.Close()

	// The inspection pass: extract whatever native text layer exists, then
	// decide from character density whether that layer is the document or a
	// thin index over scanned images (§5.4).
	text, _ := doc.ExtractText()
	class := pdf.Classify(text, doc.PageCount())

	if class.Digital {
		telemetry.PDFClassification.WithLabelValues("digital").Inc()
	} else {
		telemetry.PDFClassification.WithLabelValues("scanned").Inc()
		ocrText, err := w.ocrPages(ctx, doc, meta.DocId)
		if err != nil {
			if errors.Is(err, ocr.ErrUnavailable) {
				return Decline(meta.DocId, "OCR_UNAVAILABLE")
			}
			return WithDoc(meta.DocId, Fatal("OCR_FAILED", err))
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

// ocrPages rasterises and OCRs one page at a time, releasing each bitmap
// before rendering the next (§5.4).
//
// Pages within a document stay sequential — that is the memory discipline that
// keeps live bitmaps bounded. Parallelism comes from other lanes working other
// documents, all of them metered by the same CPU semaphore.
func (w *DocumentWorker) ocrPages(ctx context.Context, doc *pdf.Document, docID string) (string, error) {
	if !ocr.Available {
		return "", ocr.ErrUnavailable
	}
	var b strings.Builder
	for i := 0; i < doc.PageCount(); i++ {
		img, cleanup, err := w.PDF.RenderPage(ctx, doc, i, RasterDPI)
		if err != nil {
			cleanup()
			if ctx.Err() != nil {
				return b.String(), ctx.Err()
			}
			w.Log.Warn("page render failed", "doc_id", docID, "page", i, "error", err)
			continue
		}
		text, err := w.OCR.Image(ctx, img)
		cleanup() // before the next page is rendered, always
		if err != nil {
			if errors.Is(err, ocr.ErrUnavailable) {
				return "", err
			}
			w.Log.Warn("page ocr failed", "doc_id", docID, "page", i, "error", err)
			continue
		}
		if t := strings.TrimSpace(text); t != "" {
			fmt.Fprintf(&b, "%s\n\n", t)
		}
	}
	return b.String(), nil
}

func (w *DocumentWorker) HandleImage(ctx context.Context, msg jetstream.Msg) error {
	var cmd ingestionv1.ProcessImageCommand
	if err := proto.Unmarshal(msg.Data(), &cmd); err != nil {
		return Fatal("MALFORMED_COMMAND", err)
	}
	meta := cmd.Metadata
	if meta == nil || meta.Traceparent == "" {
		return Fatal("MISSING_TRACE_CONTEXT", fmt.Errorf("command carries no traceparent"))
	}

	// Dimensions were recorded at discovery, so the gate can reject without a
	// fetch (§4.1).
	if ok, why := ocr.Viable(make([]byte, cmd.ByteSize), int(cmd.Width), int(cmd.Height)); !ok {
		telemetry.Skipped.WithLabelValues(why).Inc()
		return Decline(meta.DocId, domain.ReasonImageNotViable)
	}
	if !ocr.Available {
		return Decline(meta.DocId, "OCR_UNAVAILABLE")
	}

	data, err := w.fetch(ctx, cmd.RustfsRawUri, meta.DocId)
	if err != nil {
		return err
	}
	_ = w.Docs.UpdateStatus(ctx, meta.DocId, domain.StatusProcessing, "")

	text, err := w.OCR.Bytes(ctx, data)
	if err != nil {
		if errors.Is(err, ocr.ErrUnavailable) {
			return Decline(meta.DocId, "OCR_UNAVAILABLE")
		}
		return WithDoc(meta.DocId, Fatal("OCR_FAILED", err))
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
		return nil, Fatal("BAD_OBJECT_URI", err)
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
