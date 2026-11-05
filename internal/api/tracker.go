package api

import (
	"context"
	"sync"
	"time"
)

// ActivityTracker feeds the background daemon's idle-shutdown timer.
type ActivityTracker struct {
	mu       sync.Mutex
	inflight int
	lastDone time.Time
	// quiet is closed when inflight returns to zero. It is nil while nothing
	// is in flight.
	quiet chan struct{}
}

func NewActivityTracker() *ActivityTracker {
	return &ActivityTracker{lastDone: time.Now()}
}

func (t *ActivityTracker) Begin() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.inflight == 0 {
		t.quiet = make(chan struct{})
	}
	t.inflight++
}

func (t *ActivityTracker) End() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.inflight--
	t.lastDone = time.Now()
	if t.inflight == 0 {
		close(t.quiet)
		t.quiet = nil
	}
}

// WaitQuiet blocks until no request or held activity is in flight, or ctx
// ends. In-process tests use it to know that a request's handler has returned.
func (t *ActivityTracker) WaitQuiet(ctx context.Context) error {
	t.mu.Lock()
	quiet := t.quiet
	t.mu.Unlock()
	if quiet == nil {
		return nil
	}
	select {
	case <-quiet:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// IdleFor returns how long the server has been fully quiet; zero while any
// request is in flight.
func (t *ActivityTracker) IdleFor() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.inflight > 0 {
		return 0
	}
	return time.Since(t.lastDone)
}
