package limits

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestCPUBoundsConcurrency(t *testing.T) {
	const size = 3
	c := NewCPU(size)

	var mu sync.Mutex
	concurrent, peak := 0, 0

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := c.Acquire(context.Background(), LabelOCR)
			if err != nil {
				t.Error(err)
				return
			}
			defer release()

			mu.Lock()
			concurrent++
			if concurrent > peak {
				peak = concurrent
			}
			mu.Unlock()

			time.Sleep(2 * time.Millisecond)

			mu.Lock()
			concurrent--
			mu.Unlock()
		}()
	}
	wg.Wait()

	if peak > size {
		t.Fatalf("peak concurrency %d exceeded semaphore size %d", peak, size)
	}
	if peak < 2 {
		t.Fatalf("peak concurrency %d suggests the semaphore is serializing", peak)
	}
	if got := c.InUse(); got != 0 {
		t.Fatalf("InUse() = %d after all releases, want 0", got)
	}
}

// The two CPU-heavy consumers must stay distinguishable while sharing one
// budget, because "are we saturating the machine" and "on what" are different
// questions on the dashboard.
func TestCPUTracksLabelsSeparately(t *testing.T) {
	c := NewCPU(4)
	ctx := context.Background()

	relOCR, _ := c.Acquire(ctx, LabelOCR)
	relRaster1, _ := c.Acquire(ctx, LabelRasterize)
	relRaster2, _ := c.Acquire(ctx, LabelRasterize)

	if got := c.Active(LabelOCR); got != 1 {
		t.Errorf("Active(ocr) = %d, want 1", got)
	}
	if got := c.Active(LabelRasterize); got != 2 {
		t.Errorf("Active(rasterize) = %d, want 2", got)
	}
	if got := c.InUse(); got != 3 {
		t.Errorf("InUse() = %d, want 3", got)
	}

	relOCR()
	relRaster1()
	relRaster2()

	if got := c.Active(LabelRasterize); got != 0 {
		t.Errorf("Active(rasterize) = %d after release, want 0", got)
	}
}

// Callers defer release unconditionally, so a double call must not free a slot
// twice and let the semaphore drift past its own size.
func TestCPUReleaseIsIdempotent(t *testing.T) {
	c := NewCPU(1)
	release, err := c.Acquire(context.Background(), LabelOCR)
	if err != nil {
		t.Fatal(err)
	}

	release()
	release()

	if got := c.InUse(); got != 0 {
		t.Fatalf("InUse() = %d after double release, want 0", got)
	}

	// The single slot must still be acquirable exactly once.
	if _, err := c.Acquire(context.Background(), LabelOCR); err != nil {
		t.Fatalf("re-acquire after double release: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.Acquire(ctx, LabelOCR); err == nil {
		t.Fatal("acquired a second slot from a size-1 semaphore")
	}
}

func TestCPURespectsContextCancellation(t *testing.T) {
	c := NewCPU(1)
	release, _ := c.Acquire(context.Background(), LabelOCR)
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if _, err := c.Acquire(ctx, LabelOCR); err == nil {
		t.Fatal("Acquire returned nil error while the semaphore was full")
	}
	// A cancelled acquire must not leak a slot it never got.
	if got := c.InUse(); got != 1 {
		t.Fatalf("InUse() = %d after a cancelled acquire, want 1", got)
	}
}
