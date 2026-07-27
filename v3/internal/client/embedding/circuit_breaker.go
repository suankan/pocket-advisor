package embedding

import (
	"errors"
	"sync"
	"time"
)

// ErrCircuitOpen is returned while the breaker is tripped. It is transient by
// nature: the worker naks and the message is redelivered, so a model endpoint
// restart costs latency rather than documents.
var ErrCircuitOpen = errors.New("embedding endpoint circuit open")

// Breaker stops hammering an endpoint that is already failing. Without it, a
// model restart turns into MaxDeliver redeliveries per in-flight document and
// a DLQ full of work that was never broken.
type Breaker struct {
	mu           sync.Mutex
	failures     int
	threshold    int
	cooldown     time.Duration
	openedAt     time.Time
	halfOpenOnly bool
}

func NewBreaker(threshold int, cooldown time.Duration) *Breaker {
	return &Breaker{threshold: threshold, cooldown: cooldown}
}

func (b *Breaker) Allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.failures < b.threshold {
		return nil
	}
	if time.Since(b.openedAt) < b.cooldown {
		return ErrCircuitOpen
	}
	// Cooldown elapsed: let exactly one request through to test the endpoint.
	if b.halfOpenOnly {
		return ErrCircuitOpen
	}
	b.halfOpenOnly = true
	return nil
}

func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.halfOpenOnly = false
}

func (b *Breaker) Fail() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	b.halfOpenOnly = false
	if b.failures >= b.threshold {
		b.openedAt = time.Now()
	}
}
