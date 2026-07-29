package telemetry

import (
	"sync"
	"sync/atomic"
)

// Live run state for the dashboard.
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

	active    atomic.Int64
	completed atomic.Int64
	skipped   atomic.Int64
	retried   atomic.Int64
	dlq       atomic.Int64
	pending   atomic.Int64
}

func (q *Queue) LaneStarted()  { q.active.Add(1) }
func (q *Queue) LaneFinished() { q.active.Add(-1) }

func (q *Queue) Completed()    { q.completed.Add(1) }
func (q *Queue) Skipped()      { q.skipped.Add(1) }
func (q *Queue) Retried()      { q.retried.Add(1) }
func (q *Queue) DeadLettered() { q.dlq.Add(1) }

// SetPending records queue depth, polled from the consumer rather than counted
// here — the broker is the only authority on how much work is still waiting.
func (q *Queue) SetPending(n int64) { q.pending.Store(n) }

func (q *Queue) Active() int64 { return q.active.Load() }

// Idle reports whether this pool is doing nothing and has nothing waiting.
// Drain detection is the sum of this across every queue.
func (q *Queue) Idle() bool {
	return q.active.Load() == 0 && q.pending.Load() == 0
}

type QueueSnapshot struct {
	Subject   string
	Role      string
	Lanes     int
	Active    int64
	Completed int64
	Skipped   int64
	Retried   int64
	DLQ       int64
	Pending   int64
}

func (q *Queue) Snapshot() QueueSnapshot {
	return QueueSnapshot{
		Subject:   q.Subject,
		Role:      q.Role,
		Lanes:     q.Lanes,
		Active:    q.active.Load(),
		Completed: q.completed.Load(),
		Skipped:   q.skipped.Load(),
		Retried:   q.retried.Load(),
		DLQ:       q.dlq.Load(),
		Pending:   q.pending.Load(),
	}
}

// Upload is the live state of the Tier 1 upload stage.
type Upload struct {
	total     atomic.Int64
	uploaded  atomic.Int64
	duplicate atomic.Int64
	failed    atomic.Int64
	bytes     atomic.Int64
	running   atomic.Bool
}

func (u *Upload) Start(total int64) {
	u.total.Store(total)
	u.running.Store(true)
}

func (u *Upload) Finish() { u.running.Store(false) }

func (u *Upload) AddTotal(n int64) { u.total.Add(n) }
func (u *Upload) Uploaded(n int64) { u.uploaded.Add(1); u.bytes.Add(n) }
func (u *Upload) Duplicate()       { u.duplicate.Add(1) }
func (u *Upload) Failed()          { u.failed.Add(1) }

type UploadSnapshot struct {
	Running   bool
	Total     int64
	Uploaded  int64
	Duplicate int64
	Failed    int64
	Bytes     int64
}

// Done counts every file the uploader has finished deciding about, which is
// what a progress bar measures — not just the ones whose bytes moved.
func (s UploadSnapshot) Done() int64 { return s.Uploaded + s.Duplicate + s.Failed }

func (u *Upload) Snapshot() UploadSnapshot {
	return UploadSnapshot{
		Running:   u.running.Load(),
		Total:     u.total.Load(),
		Uploaded:  u.uploaded.Load(),
		Duplicate: u.duplicate.Load(),
		Failed:    u.failed.Load(),
		Bytes:     u.bytes.Load(),
	}
}

// Stats is every live number for one run.
type Stats struct {
	Upload Upload

	mu     sync.RWMutex
	queues []*Queue
}

func NewStats() *Stats { return &Stats{} }

// RegisterQueue adds a pool to the dashboard. Registration order is display
// order, so callers register in pipeline order.
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

func (s *Stats) QueueSnapshots() []QueueSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]QueueSnapshot, 0, len(s.queues))
	for _, q := range s.queues {
		out = append(out, q.Snapshot())
	}
	return out
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

// DeadLettered totals dead-lettered work across every pool, so a run that
// finished but lost documents cannot report itself as a clean success.
func (s *Stats) DeadLettered() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var n int64
	for _, q := range s.queues {
		n += q.dlq.Load()
	}
	return n
}
