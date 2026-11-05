// Package waitlock provides a reader/writer lock whose waiters block on
// channels rather than runtime semaphores.
//
// A goroutine waiting for a sync.Mutex or sync.RWMutex is not durably blocked
// inside a testing/synctest bubble, so synctest.Wait never returns and fake
// time never advances while one waits. A goroutine waiting for an RWMutex
// from this package is durably blocked, so tests can observe an operation
// queued behind the lock without timing it.
package waitlock

import (
	"context"
	"fmt"

	"golang.org/x/sync/semaphore"
)

// exclusive is the writer's share of the lock; each reader takes one unit.
const exclusive int64 = 1 << 30

// RWMutex keeps the sync.RWMutex contract for holders: a writer excludes
// everyone, readers share, and waiters are served in arrival order, so a
// waiting writer holds back later readers. Releasing more than is held panics.
// The zero value is not usable; construct one with New.
type RWMutex struct{ gate *semaphore.Weighted }

// New returns an unlocked RWMutex.
func New() RWMutex { return RWMutex{gate: semaphore.NewWeighted(exclusive)} }

// Lock waits for exclusive ownership. It returns ctx's error, holding
// nothing, if ctx ends first.
func (l RWMutex) Lock(ctx context.Context) error {
	if err := l.gate.Acquire(ctx, exclusive); err != nil {
		return fmt.Errorf("waiting for exclusive lock: %w", err)
	}
	return nil
}

// RLock waits for a shared hold. It returns ctx's error, holding nothing, if
// ctx ends first.
func (l RWMutex) RLock(ctx context.Context) error {
	if err := l.gate.Acquire(ctx, 1); err != nil {
		return fmt.Errorf("waiting for shared lock: %w", err)
	}
	return nil
}

func (l RWMutex) Unlock()  { l.gate.Release(exclusive) }
func (l RWMutex) RUnlock() { l.gate.Release(1) }

// TryRLock takes a read lock if no writer holds or waits for the lock.
func (l RWMutex) TryRLock() bool { return l.gate.TryAcquire(1) }
