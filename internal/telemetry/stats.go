package telemetry

import (
	"sync"
	"sync/atomic"
)

// Live run state for drain detection.
//
// Deliberately separate from the Prometheus metrics above rather than derived
// from them: the interesting numbers here are point-in-time (how many lanes are
// busy right now, how deep is this queue), which counters cannot answer, and
// reading values back out of a Prometheus registry means Gather() plus decoding
// dto structs several times a second. Atomics are cheaper and exact.
//
// The two are updated side by side. Prometheus remains the record for anything
// asked after the run; this is only for what is happening during it.

// Queue is the live state of one subject's worker pool.
type Queue struct {
	Subject string
	Role    string
	Lanes   int

	active  atomic.Int64
	pending atomic.Int64
}

func (q *Queue) LaneStarted()  { q.active.Add(1) }
func (q *Queue) LaneFinished() { q.active.Add(-1) }

// SetPending records queue depth, polled from the consumer rather than counted
// here — the broker is the only authority on how much work is still waiting.
func (q *Queue) SetPending(n int64) { q.pending.Store(n) }

func (q *Queue) Active() int64 { return q.active.Load() }

// Idle reports whether this pool is doing nothing and has nothing waiting.
// Drain detection is the sum of this across every queue.
func (q *Queue) Idle() bool {
	return q.active.Load() == 0 && q.pending.Load() == 0
}

// Stats is every live number for one run.
type Stats struct {
	mu     sync.RWMutex
	queues []*Queue
}

func NewStats() *Stats { return &Stats{} }

// RegisterQueue adds a pool to the drain check.
func (s *Stats) RegisterQueue(role, subject string, lanes int) *Queue {
	q := &Queue{Subject: subject, Role: role, Lanes: lanes}
	s.mu.Lock()
	s.queues = append(s.queues, q)
	s.mu.Unlock()
	return q
}

func (s *Stats) Queues() []*Queue {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]*Queue(nil), s.queues...)
}

// Idle reports whether every pool is idle with an empty queue — the condition
// a one-shot run exits on.
func (s *Stats) Idle() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, q := range s.queues {
		if !q.Idle() {
			return false
		}
	}
	return true
}
