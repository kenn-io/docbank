package api

import "sync"

// gate serializes maintenance against regular mutations. Regular mutating
// handlers hold the read side (they may run concurrently with each other);
// gc --run, trash empty, and verify hold the write side so they observe a
// quiescent vault. Requests queue rather than fail.
type gate struct{ mu sync.RWMutex }

func (g *gate) mutate(fn func() error) error {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return fn()
}

func (g *gate) maintain(fn func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return fn()
}
