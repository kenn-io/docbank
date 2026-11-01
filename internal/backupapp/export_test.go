package backupapp

import "testing"

// SetBeforeCaptureLock runs hook after a capture starts and before Kit locks
// the repository. Callers must not run in parallel.
func SetBeforeCaptureLock(tb testing.TB, hook func()) {
	tb.Helper()
	previous := beforeCaptureLock
	beforeCaptureLock = hook
	tb.Cleanup(func() { beforeCaptureLock = previous })
}
