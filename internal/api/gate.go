package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
)

// OperationGate serializes maintenance against regular mutations and active backup
// captures. Regular mutating handlers hold mu's read side and may run
// concurrently. Maintenance holds both exclusive sides. A backup holds the
// preservation read side for its full capture, but takes mu exclusively only
// for Kit's short metadata freeze, so ordinary writes resume while maintenance
// remains queued behind the snapshot's content requirements.
type OperationGate struct {
	mu           sync.RWMutex
	preservation sync.RWMutex
	admission    sync.RWMutex
	maintenance  int
}

// NewOperationGate creates one daemon-wide operation coordinator. Every
// mutating entry point, including daemon-owned jobs, must share this instance.
func NewOperationGate() *OperationGate { return &OperationGate{} }

// Mutate runs fn as an ordinary mutation, excluding maintenance while the
// complete physical-write and metadata-publication operation is in flight.
func (g *OperationGate) Mutate(fn func() error) error {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return fn()
}

// Maintain runs daemon-owned physical maintenance with the same exclusion and
// admission behavior as an HTTP maintenance request.
func (g *OperationGate) Maintain(fn func() error) error { return g.maintain(fn) }

func (g *OperationGate) mutate(fn func() error) error {
	g.admission.RLock()
	if g.maintenance > 0 {
		g.admission.RUnlock()
		return NewError(http.StatusServiceUnavailable, "maintenance_busy",
			"vault maintenance is running or queued; retry this mutation after it finishes")
	}
	// Keep admission pinned until the shared side is held. A short backup
	// freeze can therefore delay this mutation without being mistaken for
	// maintenance, while newly queued maintenance cannot overtake it.
	g.mu.RLock()
	g.admission.RUnlock()
	defer g.mu.RUnlock()
	return fn()
}

func (g *OperationGate) maintain(fn func() error) error {
	g.admission.Lock()
	g.maintenance++
	g.admission.Unlock()
	defer func() {
		g.admission.Lock()
		g.maintenance--
		g.admission.Unlock()
	}()
	g.preservation.Lock()
	defer g.preservation.Unlock()
	g.mu.Lock()
	defer g.mu.Unlock()
	return fn()
}

func (g *OperationGate) capture(fn func() error) error {
	g.preservation.RLock()
	defer g.preservation.RUnlock()
	return fn()
}

// gateFreezer implements Kit's short freeze protocol. It takes the exclusive
// side only until the metadata source has pinned its deferred SQLite snapshot;
// content streaming then proceeds while ordinary mutations resume into WAL.
type gateFreezer struct {
	gate *OperationGate
	held bool
}

// gate keeps the route-local spelling compact while the daemon shares the
// exported coordinator with background jobs.
type gate = OperationGate

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
