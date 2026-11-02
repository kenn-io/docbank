package docbank

import (
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
)

func requireBlocked(t *testing.T, done <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-done:
		require.FailNow(t, msg)
	default:
	}
}

func TestWaitLockWriterExcludesReadersAndWriters(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		lock := newWaitLock()
		lock.Lock()
		reader, writer := make(chan struct{}), make(chan struct{})
		go func() { lock.RLock(); close(reader); lock.RUnlock() }()
		go func() { lock.Lock(); close(writer); lock.Unlock() }()
		synctest.Wait()
		requireBlocked(t, reader, "a reader entered while a writer held the lock")
		requireBlocked(t, writer, "a second writer entered while a writer held the lock")
		require.False(t, lock.TryRLock())
		lock.Unlock()
		<-reader
		<-writer
	})
}

func TestWaitLockReadersShare(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		lock := newWaitLock()
		lock.RLock()
		acquired, release := make(chan struct{}), make(chan struct{})
		go func() {
			lock.RLock()
			close(acquired)
			<-release
			lock.RUnlock()
		}()
		<-acquired
		require.True(t, lock.TryRLock(), "readers must share the lock")
		lock.RUnlock()
		close(release)
		lock.RUnlock()
		synctest.Wait()
		lock.Lock()
		lock.Unlock()
	})
}

func TestWaitLockQueuedWriterBlocksLaterReaders(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		lock := newWaitLock()
		lock.RLock()
		writerAcquired, writerRelease := make(chan struct{}), make(chan struct{})
		go func() {
			lock.Lock()
			close(writerAcquired)
			<-writerRelease
			lock.Unlock()
		}()
		synctest.Wait()
		requireBlocked(t, writerAcquired, "a writer entered while a reader held the lock")
		require.False(t, lock.TryRLock(), "a queued writer must hold back new readers")
		readerAcquired := make(chan struct{})
		go func() { lock.RLock(); close(readerAcquired); lock.RUnlock() }()
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

func TestWaitLockRejectsOverRelease(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() { newWaitLock().RUnlock() })
	require.Panics(t, func() { newWaitLock().Unlock() })
	lock := newWaitLock()
	lock.RLock()
	require.Panics(t, lock.Unlock, "releasing the writer share while only a reader holds the lock")
}
