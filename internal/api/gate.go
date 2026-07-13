package api

import (
	"context"
	"errors"
	"sync"
)

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

// gateFreezer implements Kit's short freeze protocol. It takes the exclusive
// side only until the metadata source has pinned its deferred SQLite snapshot;
// content streaming then proceeds while ordinary mutations resume into WAL.
type gateFreezer struct {
	gate *gate
	held bool
}

func (f *gateFreezer) Begin(ctx context.Context) error {
	if f.held {
		return errors.New("backup freeze is already held")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f.gate.mu.Lock()
	if err := ctx.Err(); err != nil {
		f.gate.mu.Unlock()
		return err
	}
	f.held = true
	return nil
}

func (f *gateFreezer) End(context.Context) error {
	if !f.held {
		return errors.New("backup freeze is not held")
	}
	f.held = false
	f.gate.mu.Unlock()
	return nil
}
