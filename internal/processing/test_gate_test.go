package processing

import (
	"context"
	"fmt"

	"golang.org/x/sync/semaphore"
)

type workerTestGate struct{ lock *semaphore.Weighted }

func newWorkerTestGate() *workerTestGate { return &workerTestGate{lock: semaphore.NewWeighted(1)} }

func (gate *workerTestGate) MutateContext(ctx context.Context, fn func() error) error {
	if err := gate.lock.Acquire(ctx, 1); err != nil {
		return fmt.Errorf("acquiring test operation gate: %w", err)
	}
	defer gate.lock.Release(1)
	return fn()
}

func (gate *workerTestGate) PreserveContext(ctx context.Context, fn func() error) error {
	return gate.MutateContext(ctx, fn)
}

func (gate *workerTestGate) MaintainContext(ctx context.Context, fn func() error) error {
	return gate.MutateContext(ctx, fn)
}
