package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"golang.org/x/sync/semaphore"
)

const operationGateExclusiveWeight int64 = 1 << 30

// OperationGate serializes maintenance against regular mutations and active backup
// captures. Regular mutating handlers hold mu's read side and may run
// concurrently. Maintenance holds both exclusive sides. A backup holds the
// preservation read side for its full capture, but takes mu exclusively only
// for Kit's short metadata freeze, so ordinary writes resume while maintenance
// remains queued behind the snapshot's content requirements.
type OperationGate struct {
	mu           *semaphore.Weighted
	preservation *semaphore.Weighted
	admission    sync.RWMutex
	maintenance  int
}

// NewOperationGate creates one daemon-wide operation coordinator. Every
// mutating entry point, including daemon-owned jobs, must share this instance.
func NewOperationGate() *OperationGate {
	return &OperationGate{
		mu:           semaphore.NewWeighted(operationGateExclusiveWeight),
		preservation: semaphore.NewWeighted(operationGateExclusiveWeight),
	}
}

// Mutate runs fn as an ordinary mutation, excluding maintenance while the
// complete physical-write and metadata-publication operation is in flight.
func (g *OperationGate) Mutate(fn func() error) error {
	return g.MutateContext(context.Background(), fn)
}

// MutateContext runs a daemon-owned logical mutation on the shared side of
// the maintenance gate, with cancellation while waiting for admission.
func (g *OperationGate) MutateContext(ctx context.Context, fn func() error) error {
	if err := g.mu.Acquire(ctx, 1); err != nil {
		return fmt.Errorf("acquiring mutation gate: %w", err)
	}
	defer g.mu.Release(1)
	return fn()
}

// Maintain runs daemon-owned physical maintenance with the same exclusion and
// admission behavior as an HTTP maintenance request.
func (g *OperationGate) Maintain(fn func() error) error {
	return g.maintainContext(context.Background(), fn)
}

// MaintainContext is Maintain with cancellation while waiting for exclusive
// daemon-owned maintenance admission.
func (g *OperationGate) MaintainContext(ctx context.Context, fn func() error) error {
	return g.maintainContext(ctx, fn)
}

// PhysicalMutate serializes a short physical-authority commit with backup
// preservation without blocking unrelated logical document mutations.
func (g *OperationGate) PhysicalMutate(fn func() error) error {
	if err := g.preservation.Acquire(
		context.Background(), operationGateExclusiveWeight,
	); err != nil {
		return fmt.Errorf("acquiring physical-authority gate: %w", err)
	}
	defer g.preservation.Release(operationGateExclusiveWeight)
	return fn()
}

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
	err := g.mu.Acquire(context.Background(), 1)
	g.admission.RUnlock()
	if err != nil {
		return fmt.Errorf("acquiring route mutation gate: %w", err)
	}
	defer g.mu.Release(1)
	return fn()
}

func (g *OperationGate) maintain(fn func() error) error {
	return g.maintainContext(context.Background(), fn)
}

func (g *OperationGate) maintainContext(ctx context.Context, fn func() error) error {
	g.admission.Lock()
	g.maintenance++
	g.admission.Unlock()
	defer func() {
		g.admission.Lock()
		g.maintenance--
		g.admission.Unlock()
	}()
	if err := g.preservation.Acquire(ctx, operationGateExclusiveWeight); err != nil {
		return fmt.Errorf("acquiring maintenance preservation gate: %w", err)
	}
	defer g.preservation.Release(operationGateExclusiveWeight)
	if err := g.mu.Acquire(ctx, operationGateExclusiveWeight); err != nil {
		return fmt.Errorf("acquiring maintenance mutation gate: %w", err)
	}
	defer g.mu.Release(operationGateExclusiveWeight)
	return fn()
}

func (g *OperationGate) capture(fn func() error) error {
	if err := g.preservation.Acquire(context.Background(), 1); err != nil {
		return fmt.Errorf("acquiring backup preservation gate: %w", err)
	}
	defer g.preservation.Release(1)
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
	if err := f.gate.mu.Acquire(ctx, operationGateExclusiveWeight); err != nil {
		return fmt.Errorf("acquiring backup freeze gate: %w", err)
	}
	f.held = true
	return nil
}

func (f *gateFreezer) End(context.Context) error {
	if !f.held {
		return errors.New("backup freeze is not held")
	}
	f.held = false
	f.gate.mu.Release(operationGateExclusiveWeight)
	return nil
}
