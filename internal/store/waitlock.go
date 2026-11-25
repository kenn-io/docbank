package store

import (
	"context"
	"fmt"

	"golang.org/x/sync/semaphore"
)

// exclusive is the writer's share of the lock; each reader takes one unit.
const waitLockExclusive int64 = 1 << 30

// WaitRWMutex keeps the sync.RWMutex contract for holders: a writer excludes
// everyone, readers share, and waiters are served in arrival order, so a
// waiting writer holds back later readers. Releasing more than is held panics.
// The zero value is not usable; construct one with NewWaitRWMutex.
type WaitRWMutex struct{ gate *semaphore.Weighted }

// NewWaitRWMutex returns an unlocked WaitRWMutex.
func NewWaitRWMutex() WaitRWMutex { return WaitRWMutex{gate: semaphore.NewWeighted(waitLockExclusive)} }

// Lock waits for exclusive ownership. It returns ctx's error, holding
// nothing, if ctx ends first.
func (l WaitRWMutex) Lock(ctx context.Context) error {
	if err := l.gate.Acquire(ctx, waitLockExclusive); err != nil {
		return fmt.Errorf("waiting for exclusive lock: %w", err)
	}
	return nil
}

// RLock waits for a shared hold. It returns ctx's error, holding nothing, if
// ctx ends first.
func (l WaitRWMutex) RLock(ctx context.Context) error {
	if err := l.gate.Acquire(ctx, 1); err != nil {
		return fmt.Errorf("waiting for shared lock: %w", err)
	}
	return nil
}

func (l WaitRWMutex) Unlock()  { l.gate.Release(waitLockExclusive) }
func (l WaitRWMutex) RUnlock() { l.gate.Release(1) }

// TryRLock takes a read lock if no writer holds or waits for the lock.
func (l WaitRWMutex) TryRLock() bool { return l.gate.TryAcquire(1) }
