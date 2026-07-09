package api

import (
	"sync"
	"time"
)

// ActivityTracker feeds the background daemon's idle-shutdown timer.
type ActivityTracker struct {
	mu       sync.Mutex
	inflight int
	lastDone time.Time
}

func NewActivityTracker() *ActivityTracker {
	return &ActivityTracker{lastDone: time.Now()}
}

func (t *ActivityTracker) Begin() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.inflight++
}

func (t *ActivityTracker) End() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.inflight--
	t.lastDone = time.Now()
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
