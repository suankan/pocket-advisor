// Package pipeline runs every worker role in one process.
//
// What used to be five Deployments is five pools here, started together and
// stopped together. Two things follow from that and live in this file: a
// process that hosts every role needs an explicit answer to "when is the work
// finished" (a Deployment never had to have one), and it needs an interrupt
// that stops fetching before it stops working (a pod being killed never had to
// care).
package pipeline

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/suankan/pocket-advisor/internal/app"
	"github.com/suankan/pocket-advisor/internal/bus"
	"github.com/suankan/pocket-advisor/internal/client/embedding"
	"github.com/suankan/pocket-advisor/internal/engine/ocr"
	"github.com/suankan/pocket-advisor/internal/engine/pdf"
	"github.com/suankan/pocket-advisor/internal/limits"
	"github.com/suankan/pocket-advisor/internal/telemetry"
	"github.com/suankan/pocket-advisor/internal/worker"
)

// role binds one subject to its pool.
type role struct {
	name     string
	subject  string
	durable  string
	lanes    int
	handler  worker.Handler
	queue    *telemetry.Queue
	consumer jetstream.Consumer
}

type Pipeline struct {
	app      *app.App
	stats    *telemetry.Stats
	embedder *embedding.Client

	pdfEngine *pdf.Engine
	ocrEngine *ocr.Engine

	roles []*role
	wg    sync.WaitGroup
}

// Options carries what the pipeline cannot derive for itself.
type Options struct {
	OCRLangs string
}

// New builds every pool and its consumer.
//
// Engines are constructed once and shared by every lane, which is the point of
// the shared CPU semaphore: one bound, not one per pool.
func New(ctx context.Context, a *app.App, stats *telemetry.Stats, embedder *embedding.Client, opts Options) (*Pipeline, error) {
	p := &Pipeline{app: a, stats: stats, embedder: embedder}

	// The instance pool is sized to the PDF lane count because Open() holds an
	// instance for a whole document, not just while rendering.
	pdfEngine, err := pdf.NewEngine(limits.DocumentLanes(), a.CPU)
	if err != nil {
		return nil, fmt.Errorf("pdf engine: %w", err)
	}
	p.pdfEngine = pdfEngine
	p.ocrEngine = ocr.NewEngine(a.CPU, opts.OCRLangs)

	if !ocr.Available {
		// Loud, because scanned PDFs and images will be recorded SKIPPED rather
		// than indexed, and that must not be discovered by surprise.
		a.Log.Warn("OCR NOT LINKED: built without -tags ocr; " +
			"scanned PDFs and images will be skipped, not indexed")
	}

	email := &worker.EmailWorker{Vault: a.Vault, Docs: a.Docs, Bus: a.Bus, Log: a.Logger(telemetry.RoleEmail)}
	docs := &worker.DocumentWorker{
		Vault: a.Vault, Docs: a.Docs, Bus: a.Bus,
		PDF: pdfEngine, OCR: p.ocrEngine, Log: a.Logger(telemetry.RoleDocument),
	}
	office := &worker.OfficeWorker{Vault: a.Vault, Docs: a.Docs, Bus: a.Bus, Log: a.Logger(telemetry.RoleOffice)}
	embed := &worker.EmbedWorker{Docs: a.Docs, Chunks: a.Chunks, Embedder: embedder, Log: a.Logger(telemetry.RoleEmbed)}

	// Registration order is display order on the dashboard, so these follow the
	// pipeline rather than the alphabet.
	specs := []*role{
		{name: telemetry.RoleEmail, subject: bus.SubjectEmails, durable: "email-processor",
			lanes: limits.EmailLanes(), handler: email.Handle},
		{name: telemetry.RoleDocument, subject: bus.SubjectPDFs, durable: "document-extractor-pdf",
			lanes: limits.DocumentLanes(), handler: docs.HandlePDF},
		{name: telemetry.RoleOffice, subject: bus.SubjectDocx, durable: "office-extractor",
			lanes: limits.OfficeLanes(), handler: office.Handle},
		// Images get their own pool rather than sharing the PDF one. They hold
		// no PDFium instance, so their lanes are cheap, and keeping them
		// separate means a queue of scanned PDFs cannot starve them — the CPU
		// semaphore still caps what both together can burn.
		{name: telemetry.RoleDocument, subject: bus.SubjectImages, durable: "document-extractor-img",
			lanes: limits.DocumentLanes(), handler: docs.HandleImage},
		{name: telemetry.RoleEmbed, subject: bus.SubjectEmbed, durable: "embed-indexer",
			lanes: limits.EmbedLanes(), handler: embed.Handle},
	}

	for _, r := range specs {
		c, err := a.Bus.PullConsumer(ctx, r.durable, r.subject)
		if err != nil {
			return nil, fmt.Errorf("consumer %s: %w", r.durable, err)
		}
		r.consumer = c
		r.queue = stats.RegisterQueue(r.name, r.subject, r.lanes)
		p.roles = append(p.roles, r)
	}

	a.Log.Info("pipeline built",
		"cpus", limits.CPUs,
		"cpu_slots", a.CPU.Size(),
		"email_lanes", limits.EmailLanes(),
		"document_lanes", limits.DocumentLanes(),
		"office_lanes", limits.OfficeLanes(),
		"embed_lanes", limits.EmbedLanes(),
		"embedding_sessions", embedder.Concurrency())
	return p, nil
}

// Start launches every pool.
//
// fetchCtx ending stops new work being pulled; workCtx bounds the handlers and
// must outlive it. Start returns immediately — use Wait to join.
func (p *Pipeline) Start(fetchCtx, workCtx context.Context) {
	for _, r := range p.roles {
		r := r
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			rt := &worker.Runtime{
				Name:    r.name,
				Bus:     p.app.Bus,
				Docs:    p.app.Docs,
				Log:     p.app.Logger(r.name),
				Subject: r.subject,
				Lanes:   r.lanes,
				Stats:   r.queue,
			}
			if err := rt.Consume(fetchCtx, workCtx, r.consumer, r.handler); err != nil {
				p.app.Logger(r.name).Error("consume stopped", "subject", r.subject, "error", err)
			}
		}()
	}
	go p.pollDepth(workCtx)
}

// Wait blocks until every pool has drained and exited.
func (p *Pipeline) Wait() { p.wg.Wait() }

// pollDepth keeps queue depth current on the dashboard.
//
// Depth comes from the consumer rather than being counted here: the broker is
// the only thing that knows what is still waiting, including work this process
// has not seen and redeliveries it has not received yet.
func (p *Pipeline) pollDepth(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		p.samplePending(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *Pipeline) samplePending(ctx context.Context) {
	for _, r := range p.roles {
		info, err := r.consumer.Info(ctx)
		if err != nil {
			continue
		}
		// NumAckPending is work already handed out and not yet acked. Counting
		// it as outstanding is what stops a drain check from declaring victory
		// while lanes in another pool are still mid-document.
		r.queue.SetPending(int64(info.NumPending) + int64(info.NumAckPending))
	}
}

// WaitDrained blocks until the pipeline has been idle for settle, or ctx ends.
//
// A Deployment never needed this: it ran until something killed it. A one-shot
// run does, and "idle right now" is not good enough as a stopping rule —
// finishing an email publishes attachment work that has not reached its queue
// yet, so the pipeline passes through empty on its way to being busy. Requiring
// the quiet to hold for a settling period is what tells those apart.
func (p *Pipeline) WaitDrained(ctx context.Context, settle time.Duration) error {
	const tick = 500 * time.Millisecond

	quietSince := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(tick):
		}

		p.samplePending(ctx)

		if !p.stats.Idle() {
			quietSince = time.Time{}
			continue
		}
		if quietSince.IsZero() {
			quietSince = time.Now()
			continue
		}
		if time.Since(quietSince) >= settle {
			return nil
		}
	}
}

func (p *Pipeline) Close() {
	if p.pdfEngine != nil {
		_ = p.pdfEngine.Close()
	}
	if p.ocrEngine != nil {
		_ = p.ocrEngine.Close()
	}
}
