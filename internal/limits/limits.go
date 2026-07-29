// Package limits derives the concurrency of every pool from the machine rather
// than from configuration.
//
// When each role ran as its own Deployment, parallelism was the replica count
// and the numbers lived in Helm values. Collapsed into a single host process,
// the only honest source for "how much can we do at once" is the host itself,
// so nothing here is a tunable (ingestion-design.md §5.4, §6.2).
package limits

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
)

// CPUs is the parallelism the machine actually offers.
var CPUs = runtime.NumCPU()

// Lane counts per role. A lane is one in-flight message, not one core: lanes
// spend most of their life blocked on object-store and database I/O, and the
// genuinely CPU-bound fraction is bounded separately by CPU below. Sizing lanes
// at core count would leave the machine idle waiting on RustFS.
func EmailLanes() int  { return 2 * CPUs }
func EmbedLanes() int  { return 2 * CPUs }
func OfficeLanes() int { return CPUs }

// DocumentLanes stays at core count rather than double it, because every lane
// holding an open PDF also holds a PDFium WebAssembly instance for the duration
// — an allocation heavy enough that oversubscribing lanes costs memory without
// buying throughput the CPU bound would allow anyway.
func DocumentLanes() int { return CPUs }

// Labels for the CPU semaphore, so the two competing consumers stay
// distinguishable on the dashboard.
const (
	LabelOCR       = "ocr"
	LabelRasterize = "rasterize"
)

// CPU bounds the genuinely CPU-bound work — PDF rasterization and OCR — across
// the whole process.
//
// One semaphore covering both rather than one each: they burn the same cores,
// so two independent bounds would oversubscribe the machine by their sum. This
// replaces both the PDF engine's process-wide render mutex, which would have
// collapsed to a single global lane once every role shared one process, and the
// OCR engine's private limit of 2, which was sized for a 1-core container.
type CPU struct {
	ch     chan struct{}
	size   int
	inUse  atomic.Int64
	labels map[string]*atomic.Int64
}

func NewCPU(size int) *CPU {
	if size < 1 {
		size = 1
	}
	return &CPU{
		ch:   make(chan struct{}, size),
		size: size,
		// Fixed at construction so the map is read-only afterwards and needs no
		// lock on the hot path.
		labels: map[string]*atomic.Int64{
			LabelOCR:       new(atomic.Int64),
			LabelRasterize: new(atomic.Int64),
		},
	}
}

// Acquire blocks until a slot frees or ctx is cancelled. The returned release
// is safe to call more than once, so callers can defer it unconditionally.
func (c *CPU) Acquire(ctx context.Context, label string) (func(), error) {
	select {
	case c.ch <- struct{}{}:
	case <-ctx.Done():
		return func() {}, ctx.Err()
	}

	counter := c.labels[label]
	c.inUse.Add(1)
	if counter != nil {
		counter.Add(1)
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			if counter != nil {
				counter.Add(-1)
			}
			c.inUse.Add(-1)
			<-c.ch
		})
	}, nil
}

func (c *CPU) Size() int  { return c.size }
func (c *CPU) InUse() int { return int(c.inUse.Load()) }

// Active reports how many slots a single consumer holds right now.
func (c *CPU) Active(label string) int {
	if counter := c.labels[label]; counter != nil {
		return int(counter.Load())
	}
	return 0
}
