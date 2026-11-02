package posthog

import (
	"sync"
	"time"
)

// SendGroup runs sends in the background and bounds the wait for completion.
// Its zero value is ready to use. A SendGroup must not be copied after first use.
// Callers can wrap PostEvent in Go with their own request timeout, then call
// Wait(50 * time.Millisecond) at exit by convention.
// Before each PostEvent send, check Eligible(prefix, version) with the forwarding
// process's prefix and version.
type SendGroup struct {
	mu       sync.Mutex
	inFlight int
	done     chan struct{}
}

// Go starts send in a goroutine. The caller owns its timeout and result handling.
func (g *SendGroup) Go(send func()) {
	g.mu.Lock()
	if g.inFlight == 0 {
		g.done = make(chan struct{})
	}
	g.inFlight++
	g.mu.Unlock()

	go func() {
		defer func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.inFlight--
			if g.inFlight == 0 {
				close(g.done)
			}
		}()
		send()
	}()
}

// Wait waits up to limit for active sends, returning immediately if none remain.
// Schedule sends before the exit wait. Wait leaves unfinished sends running.
func (g *SendGroup) Wait(limit time.Duration) {
	g.mu.Lock()
	if g.inFlight == 0 {
		g.mu.Unlock()
		return
	}
	done := g.done
	g.mu.Unlock()

	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}
