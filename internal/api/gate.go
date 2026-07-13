package api

import (
	"context"
	"errors"
	"sync"
)

// gate serializes maintenance against regular mutations and active backup
// captures. Regular mutating handlers hold mu's read side and may run
// concurrently. Maintenance holds both exclusive sides. A backup holds the
// preservation read side for its full capture, but takes mu exclusively only
// for Kit's short metadata freeze, so ordinary writes resume while maintenance
// remains queued behind the snapshot's content requirements.
type gate struct {
	mu           sync.RWMutex
	preservation sync.RWMutex
}

func (g *gate) mutate(fn func() error) error {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return fn()
}

func (g *gate) maintain(fn func() error) error {
	g.preservation.Lock()
	defer g.preservation.Unlock()
	g.mu.Lock()
	defer g.mu.Unlock()
	return fn()
}

func (g *gate) capture(fn func() error) error {
	g.preservation.RLock()
	defer g.preservation.RUnlock()
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
