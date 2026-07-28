// Command document-extractor handles PDFs and standalone images.
//
// Consumes both ingest.pdfs.raw and ingest.images.raw: one pool, one bounded
// OCR semaphore, one memory ceiling (ingestion-design.md §5.4).
package main

import (
	"flag"
	"fmt"
	"os"
	"sync"

	"github.com/suankan/pocket-advisor/internal/app"
	"github.com/suankan/pocket-advisor/internal/bus"
	"github.com/suankan/pocket-advisor/internal/engine/ocr"
	"github.com/suankan/pocket-advisor/internal/engine/pdf"
	"github.com/suankan/pocket-advisor/internal/worker"
)

func main() {
	var (
		ocrConcurrency = flag.Int("ocr-concurrency", 2, "process-wide OCR limit")
		pdfInstances   = flag.Int("pdf-instances", 2, "pdfium instance pool size")
		langs          = flag.String("ocr-langs", "eng+rus", "tesseract languages")
	)
	flag.Parse()

	a, err := app.New("document-extractor", app.Needs{MinIO: true, Postgres: true, NATS: true})
	if err != nil {
		fmt.Fprintf(os.Stderr, "document-extractor: %v\n", err)
		os.Exit(1)
	}
	defer a.Close()

	pdfEngine, err := pdf.NewEngine(*pdfInstances)
	if err != nil {
		a.Log.Error("pdf engine", "error", err)
		os.Exit(1)
	}
	defer pdfEngine.Close()

	ocrEngine := ocr.NewEngine(*ocrConcurrency, *langs)
	defer ocrEngine.Close()

	if !ocr.Available {
		// Loud, because scanned PDFs and images will be recorded SKIPPED
		// rather than indexed, and that must not be discovered by surprise.
		a.Log.Warn("OCR NOT LINKED: built without -tags ocr; " +
			"scanned PDFs and images will be skipped, not indexed")
	}

	w := &worker.DocumentWorker{
		Vault: a.Vault, Docs: a.Docs, Bus: a.Bus,
		PDF: pdfEngine, OCR: ocrEngine, Log: a.Log,
	}

	pdfConsumer, err := a.Bus.PullConsumer(a.Ctx, "document-extractor-pdf", bus.SubjectPDFs)
	if err != nil {
		a.Log.Error("pdf consumer", "error", err)
		os.Exit(1)
	}
	imgConsumer, err := a.Bus.PullConsumer(a.Ctx, "document-extractor-img", bus.SubjectImages)
	if err != nil {
		a.Log.Error("image consumer", "error", err)
		os.Exit(1)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// CPU bound: fetch one task at a time so idle workers steal from busy
	// ones rather than sitting on a queued batch (§5.4).
	go func() {
		defer wg.Done()
		rt := &worker.Runtime{
			Name: "document-extractor", Bus: a.Bus, Docs: a.Docs, Log: a.Log,
			Subject: bus.SubjectPDFs, Batch: 1,
		}
		if err := rt.Consume(a.Ctx, pdfConsumer, w.HandlePDF); err != nil {
			a.Log.Error("pdf consume", "error", err)
		}
	}()

	go func() {
		defer wg.Done()
		rt := &worker.Runtime{
			Name: "document-extractor", Bus: a.Bus, Docs: a.Docs, Log: a.Log,
			Subject: bus.SubjectImages, Batch: 1,
		}
		if err := rt.Consume(a.Ctx, imgConsumer, w.HandleImage); err != nil {
			a.Log.Error("image consume", "error", err)
		}
	}()

	wg.Wait()
	a.Log.Info("shutdown complete")
}
