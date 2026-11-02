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

	"golang.org/x/sync/semaphore"
)

// exclusive is the writer's share of the lock; each reader takes one unit.
const exclusive int64 = 1 << 30

// RWMutex keeps the sync.RWMutex contract: a writer excludes everyone,
// readers share, waits cannot be canceled, and waiters are served in arrival
// order, so a waiting writer holds back later readers. Releasing more than is
// held panics. The zero value is not usable; construct one with New.
type RWMutex struct{ gate *semaphore.Weighted }

// New returns an unlocked RWMutex.
func New() RWMutex { return RWMutex{gate: semaphore.NewWeighted(exclusive)} }

// Lock and RLock wait without a context, like sync.RWMutex. Acquire only
// fails when its context ends, and the background context never does.
func (l RWMutex) Lock()  { _ = l.gate.Acquire(context.Background(), exclusive) }
func (l RWMutex) RLock() { _ = l.gate.Acquire(context.Background(), 1) }

func (l RWMutex) Unlock()  { l.gate.Release(exclusive) }
func (l RWMutex) RUnlock() { l.gate.Release(1) }

// TryRLock takes a read lock if no writer holds or waits for the lock.
func (l RWMutex) TryRLock() bool { return l.gate.TryAcquire(1) }
