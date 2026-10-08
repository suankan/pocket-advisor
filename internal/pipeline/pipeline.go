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

	"github.com/suankan/pocket-advisor/internal/app"
	"github.com/suankan/pocket-advisor/internal/client/embedding"
	"github.com/suankan/pocket-advisor/internal/engine/ocr"
	"github.com/suankan/pocket-advisor/internal/engine/pdf"
	"github.com/suankan/pocket-advisor/internal/limits"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/telemetry"
	"github.com/suankan/pocket-advisor/internal/worker"
)

// role binds one subject to its pool.
type role struct {
	name    string
	subject string
	lanes   int
	handler worker.Handler
	queue   *telemetry.Queue
	runtime *worker.Runtime
}

type Pipeline struct {
	app      *app.App
	stats    *telemetry.Stats
	embedder *embedding.Client

	pdfEngine *pdf.Engine
	ocrEngine *ocr.Engine

	roles []*role
	wg    sync.WaitGroup

	// aux is the sampler and the listener, joined by Wait after the pools.
	aux      sync.WaitGroup
	stopAux  context.CancelFunc
	listener *postgres.Listener
	auxOnce  sync.Once
}

// Options carries what the pipeline cannot derive for itself.
type Options struct {
	OCRLangs string
}

// New builds every pool.
//
// Engines are constructed once and shared by every lane, which is the point of
// the shared CPU semaphore: one bound, not one per pool.
func New(ctx context.Context, a *app.App, stats *telemetry.Stats, embedder *embedding.Client, opts Options) (*Pipeline, error) {
	if a.DB.Pool.Config().MaxConns < 4 {
		return nil, fmt.Errorf("ingestion requires postgres.max_conns >= 4")
	}
	p := &Pipeline{app: a, stats: stats, embedder: embedder}

	// Cheap now: the instance pool is built on first use, so this costs
	// nothing until a PDF actually arrives (deviation 24).
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

	email := &worker.EmailWorker{
		Sources: a.Sources, Inventory: a.Inventory, Docs: a.Docs, Emails: a.Emails, Bus: a.Jobs,
		Log: a.Logger(telemetry.RoleEmail),
	}
	docs := &worker.DocumentWorker{
		Sources: a.Sources, Docs: a.Docs, Bus: a.Jobs,
		PDF: pdfEngine, OCR: p.ocrEngine, Log: a.Logger(telemetry.RoleDocument),
	}
	office := &worker.OfficeWorker{Sources: a.Sources, Docs: a.Docs, Bus: a.Jobs, Log: a.Logger(telemetry.RoleOffice)}
	embed := &worker.EmbedWorker{Docs: a.Docs, Chunks: a.Chunks, Embedder: embedder, Log: a.Logger(telemetry.RoleEmbed)}

	// Roles follow the pipeline rather than the alphabet.
	specs := []*role{
		{name: telemetry.RoleEmail, subject: postgres.SubjectEmails,
			lanes: limits.EmailLanes(), handler: email.Handle},
		{name: telemetry.RoleDocument, subject: postgres.SubjectPDFs,
			lanes: limits.DocumentLanes(), handler: docs.HandlePDF},
		{name: telemetry.RoleOffice, subject: postgres.SubjectDocx,
			lanes: limits.OfficeLanes(), handler: office.Handle},
		// Images get their own pool rather than sharing the PDF one. They hold
		// no PDFium instance, so their lanes are cheap, and keeping them
		// separate means a queue of scanned PDFs cannot starve them — the CPU
		// semaphore still caps what both together can burn.
		{name: telemetry.RoleDocument, subject: postgres.SubjectImages,
			lanes: limits.DocumentLanes(), handler: docs.HandleImage},
		{name: telemetry.RoleEmbed, subject: postgres.SubjectEmbed,
			lanes: limits.EmbedLanes(), handler: embed.Handle},
	}
	for _, r := range specs {
		r.queue = stats.RegisterQueue(r.name, r.subject, r.lanes)
		r.runtime = &worker.Runtime{
			Name:    r.name,
			Queue:   a.Jobs,
			Gate:    a.Jobs.Gate(),
			Docs:    a.Docs,
			Log:     a.Logger(r.name),
			Subject: r.subject,
			Lanes:   r.lanes,
			Stats:   r.queue,
		}
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
// fetchCtx ending stops new work being claimed; workCtx bounds the handlers and
// must outlive it. Start returns immediately — use Wait to join. LISTEN is
// established before the first claim so no notification can be missed; if it
// cannot be, polling carries the work while the listener keeps reconnecting.
func (p *Pipeline) Start(fetchCtx, workCtx context.Context) {
	auxCtx, stop := context.WithCancel(workCtx)
	p.stopAux = stop

	log := p.app.Logger(telemetry.RoleApp)
	p.listener = p.app.DB.NewListener(p.wakeAll, func(msg string, args ...any) { log.Warn(msg, args...) })
	p.listener.Start(auxCtx)

	for _, r := range p.roles {
		r := r
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			if err := r.runtime.Consume(fetchCtx, workCtx, r.handler); err != nil {
				p.app.Logger(r.name).Error("consume stopped", "subject", r.subject, "error", err)
			}
		}()
	}
	p.aux.Add(1)
	go func() {
		defer p.aux.Done()
		p.pollDepth(auxCtx)
	}()
}

// wakeAll broadcasts a notification to every pool: one shared consumable
// signal would wake only one role.
func (p *Pipeline) wakeAll() {
	for _, r := range p.roles {
		r.runtime.Wake()
	}
}

// Wait blocks until every pool has drained and exited, then stops and joins the
// depth sampler and the listener.
func (p *Pipeline) Wait() {
	p.wg.Wait()
	p.auxOnce.Do(func() {
		if p.stopAux != nil {
			p.stopAux()
		}
		p.aux.Wait()
		if p.listener != nil {
			p.listener.Close()
		}
	})
}

// pollDepth keeps queue depth and due times current for drain detection and
// wake-ups, from one grouped query per tick.
//
// Depth comes from the job table rather than being counted here: the table is
// the only thing that knows what is still waiting, including delayed retries
// and work this process has not claimed yet.
func (p *Pipeline) pollDepth(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		if err := p.samplePending(ctx); err != nil && ctx.Err() == nil {
			p.app.Log.Debug("depth sample failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// samplePending takes one depth sample and publishes it: pending work to the
// drain check, and each subject's next due time (converted from database to
// local time) to its pool. Errors are returned so drain detection can refuse
// to treat a failed sample as an empty queue.
func (p *Pipeline) samplePending(ctx context.Context) error {
	depths, err := p.app.Jobs.Depths(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, r := range p.roles {
		d := depths[r.subject]
		// READY plus RUNNING: counting running work as outstanding is what
		// stops a drain check from declaring victory while lanes in another
		// pool are still mid-document.
		r.queue.SetPending(int64(d.Pending))
		var due time.Time
		if !d.NextReady.IsZero() {
			due = now.Add(d.NextReady.Sub(d.ServerNow))
		}
		if !d.NextExpiry.IsZero() {
			if e := now.Add(d.NextExpiry.Sub(d.ServerNow)); due.IsZero() || e.Before(due) {
				due = e
			}
		}
		r.runtime.SetDue(due)
	}
	return nil
}

// WaitDrained blocks until the pipeline has been idle for settle, or ctx ends.
//
// A Deployment never needed this: it ran until something killed it. A one-shot
// run does, and "idle right now" is not good enough as a stopping rule —
// finishing an email publishes attachment work that has not reached its queue
// yet, so the pipeline passes through empty on its way to being busy. Requiring
// the quiet to hold for a settling period is what tells those apart. Delayed
// retries and expired leases count as unfinished, and a failed sample is not an
// empty observation.
func (p *Pipeline) WaitDrained(ctx context.Context, settle time.Duration) error {
	const tick = 500 * time.Millisecond

	quietSince := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(tick):
		}

		if err := p.samplePending(ctx); err != nil {
			quietSince = time.Time{}
			p.app.Log.Debug("drain sample failed; observation discarded", "error", err)
			continue
		}

		ready, unfinished, _, _, err := p.app.Jobs.Counts(ctx)
		if err != nil {
			return err
		}
		if !p.stats.Idle() || ready > 0 || unfinished > 0 {
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
