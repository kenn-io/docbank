package waitlock

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
)

func mustLock(t *testing.T, lock RWMutex) {
	t.Helper()
	if err := lock.Lock(t.Context()); err != nil {
		t.Errorf("Lock: %v", err)
	}
}

func mustRLock(t *testing.T, lock RWMutex) {
	t.Helper()
	if err := lock.RLock(t.Context()); err != nil {
		t.Errorf("RLock: %v", err)
	}
}

func requireBlocked(t *testing.T, done <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-done:
		require.FailNow(t, msg)
	default:
	}
}

func TestWriterExcludesReadersAndWriters(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		lock := New()
		mustLock(t, lock)
		reader, writer := make(chan struct{}), make(chan struct{})
		go func() { mustRLock(t, lock); close(reader); lock.RUnlock() }()
		go func() { mustLock(t, lock); close(writer); lock.Unlock() }()
		synctest.Wait()
		requireBlocked(t, reader, "a reader entered while a writer held the lock")
		requireBlocked(t, writer, "a second writer entered while a writer held the lock")
		require.False(t, lock.TryRLock())
		lock.Unlock()
		<-reader
		<-writer
	})
}

func TestReadersShare(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		lock := New()
		mustRLock(t, lock)
		acquired, release := make(chan struct{}), make(chan struct{})
		go func() {
			mustRLock(t, lock)
			close(acquired)
			<-release
			lock.RUnlock()
		}()
		<-acquired
		require.True(t, lock.TryRLock(), "readers must share the lock")
		lock.RUnlock()
		close(release)
		lock.RUnlock()
		writer := make(chan struct{})
		go func() { mustLock(t, lock); close(writer); lock.Unlock() }()
		<-writer
	})
}

func TestQueuedWriterBlocksLaterReaders(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		lock := New()
		mustRLock(t, lock)
		writerAcquired, writerRelease := make(chan struct{}), make(chan struct{})
		go func() {
			mustLock(t, lock)
			close(writerAcquired)
			<-writerRelease
			lock.Unlock()
		}()
		synctest.Wait()
		requireBlocked(t, writerAcquired, "a writer entered while a reader held the lock")
		require.False(t, lock.TryRLock(), "a queued writer must hold back new readers")
		readerAcquired := make(chan struct{})
		go func() { mustRLock(t, lock); close(readerAcquired); lock.RUnlock() }()
		synctest.Wait()
		requireBlocked(t, readerAcquired, "a reader overtook a queued writer")

		lock.RUnlock()
		<-writerAcquired
		synctest.Wait()
		requireBlocked(t, readerAcquired, "a reader entered while the writer held the lock")
		close(writerRelease)
		<-readerAcquired
	})
}

func TestRejectsOverRelease(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() { New().RUnlock() })
	require.Panics(t, func() { New().Unlock() })
	lock := New()
	mustRLock(t, lock)
	require.Panics(t, lock.Unlock, "releasing the writer share while only a reader holds the lock")
}

func TestCanceledWaitHoldsNothing(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		lock := New()
		mustLock(t, lock)
		ctx, cancel := context.WithCancel(t.Context())
		waited := make(chan error, 2)
		go func() { waited <- lock.Lock(ctx) }()
		go func() { waited <- lock.RLock(ctx) }()
		synctest.Wait()
		cancel()
		require.ErrorIs(t, <-waited, context.Canceled)
		require.ErrorIs(t, <-waited, context.Canceled)
		lock.Unlock()
		require.True(t, lock.TryRLock(), "a canceled waiter must not keep a hold")
		lock.RUnlock()
	})
}
