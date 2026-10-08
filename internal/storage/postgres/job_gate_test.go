package postgres

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGateNeverReservesMoreThanItHas(t *testing.T) {
	g := NewGate(5)
	if got := g.TryReserve(100); got != 5 {
		t.Fatalf("reserved %d of an oversized request, want the whole capacity of 5", got)
	}
	if got := g.TryReserve(1); got != 0 {
		t.Fatalf("reserved %d from an empty gate", got)
	}
	g.Release(2)
	if got := g.TryReserve(4); got != 2 {
		t.Fatalf("reserved %d with 2 free, want a partial 2 without waiting", got)
	}
}

func TestGateReleaseNeverExceedsTotalCapacity(t *testing.T) {
	g := NewGate(3)
	g.Release(10)
	if g.Free() != 3 {
		t.Fatalf("free = %d after an over-release, want 3", g.Free())
	}
}

func TestGateIgnoresNonPositiveRequests(t *testing.T) {
	g := NewGate(0) // a degenerate size still yields one slot
	if g.Size() != 1 {
		t.Fatalf("size = %d, want 1", g.Size())
	}
	if g.TryReserve(0) != 0 || g.TryReserve(-3) != 0 {
		t.Fatal("a non-positive request reserved capacity")
	}
	g.Release(0)
	g.Release(-1)
	if g.Free() != 1 {
		t.Fatalf("free = %d", g.Free())
	}
}

func TestGateReleaseWakesEveryWaiter(t *testing.T) {
	g := NewGate(1)
	g.TryReserve(1)
	var woken atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		ch := g.Wait() // taken before the release, as a pool takes it before reserving
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-ch:
				woken.Add(1)
			case <-time.After(2 * time.Second):
			}
		}()
	}
	g.Release(1)
	wg.Wait()
	if woken.Load() != 4 {
		t.Fatalf("%d of 4 waiters woke on a release", woken.Load())
	}
}

// A channel taken after a release must wait for the next one, not fire on the
// release that already happened.
func TestGateWaitChannelIsFreshAfterEachRelease(t *testing.T) {
	g := NewGate(1)
	g.TryReserve(1)
	g.Release(1)
	select {
	case <-g.Wait():
		t.Fatal("a stale release woke a new waiter")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestGateHoldsUnderContention(t *testing.T) {
	const capacity = 4
	g := NewGate(capacity)
	var held, peak atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				n := g.TryReserve(3)
				if n == 0 {
					continue
				}
				now := held.Add(int64(n))
				for {
					p := peak.Load()
					if now <= p || peak.CompareAndSwap(p, now) {
						break
					}
				}
				held.Add(-int64(n))
				g.Release(n)
			}
		}()
	}
	wg.Wait()
	if peak.Load() > capacity {
		t.Fatalf("%d slots held at once from a gate of %d", peak.Load(), capacity)
	}
	if g.Free() != capacity {
		t.Fatalf("free = %d at rest, want %d: a slot leaked", g.Free(), capacity)
	}
}
