package postgres

import "sync"

// Gate bounds how many stage transactions are open at once, process-wide. It
// is a counting reservation, not a blocking semaphore: a pool takes only the
// capacity that is free right now and never waits while holding a partial
// batch, so two pools can never each hold half of what they need.
type Gate struct {
	mu    sync.Mutex
	free  int
	total int
	// released is closed and replaced on every Release, waking whoever holds
	// the channel Wait returned.
	released chan struct{}
}

func NewGate(total int) *Gate {
	if total < 1 {
		total = 1
	}
	return &Gate{free: total, total: total, released: make(chan struct{})}
}

// Size is the total capacity.
func (g *Gate) Size() int { return g.total }

// TryReserve takes up to n free slots without blocking and reports how many it
// got. It never returns more than the gate's total capacity.
func (g *Gate) TryReserve(n int) int {
	if n < 1 {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	n = min(n, g.total, g.free)
	g.free -= n
	return n
}

// Release returns n slots and wakes every waiter.
func (g *Gate) Release(n int) {
	if n < 1 {
		return
	}
	g.mu.Lock()
	g.free = min(g.free+n, g.total)
	close(g.released)
	g.released = make(chan struct{})
	g.mu.Unlock()
}

// Wait returns a channel closed by the next Release. Take it before calling
// TryReserve, so a release between the two is not missed.
func (g *Gate) Wait() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.released
}

// Free is the number of unreserved slots.
func (g *Gate) Free() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.free
}
