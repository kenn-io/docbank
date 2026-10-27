package mcp

import (
	"context"
	"net/http"
	"sync"
)

// RequestGate gives MCP handlers served by a shared listener the same
// shutdown contract as a dedicated MCP listener: stop admission, drain for a
// bounded grace period, then cancel in-flight handler contexts.
type RequestGate struct {
	base     context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	stopped  bool
	inflight sync.WaitGroup
}

// NewRequestGate returns a gate that admits requests until Shutdown.
func NewRequestGate() *RequestGate {
	base, cancel := context.WithCancel(context.Background())
	return &RequestGate{base: base, cancel: cancel}
}

// Wrap admits requests to next until Shutdown starts.
func (g *RequestGate) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		if g.stopped {
			g.mu.Unlock()
			http.Error(w, "MCP is shutting down", http.StatusServiceUnavailable)
			return
		}
		g.inflight.Add(1)
		g.mu.Unlock()
		defer g.inflight.Done()

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		stop := context.AfterFunc(g.base, cancel)
		defer stop()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Shutdown rejects new requests and waits for admitted ones. When ctx ends
// first, it cancels their contexts and returns ctx's error.
func (g *RequestGate) Shutdown(ctx context.Context) error {
	defer g.cancel()
	g.mu.Lock()
	g.stopped = true
	g.mu.Unlock()
	done := make(chan struct{})
	go func() {
		g.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
