//go:build windows

package push_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/push"
)

// A one-shot push never scans again, so a file another program holds open must
// be reported rather than skipped.
func TestPushReportsAFileAnotherProgramHoldsOpen(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := t.TempDir()
	held := filepath.Join(dir, "held.txt")
	require.NoError(t, os.WriteFile(held, []byte("held"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "free.txt"), []byte("free"), 0o600))
	name, err := windows.UTF16PtrFromString(held)
	require.NoError(t, err)
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, 0, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	require.NoError(t, err)
	defer func() { require.NoError(t, windows.CloseHandle(handle)) }()

	opts := options(dir)
	var failed []string
	opts.Failure = func(ref string, err error) error {
		failed = append(failed, ref)
		assert.ErrorIs(t, err, windows.ERROR_SHARING_VIOLATION)
		return nil
	}
	report, err := push.Run(t.Context(), f.connection, opts)
	require.ErrorIs(t, err, push.ErrFilesFailed)
	assert.Equal(t, 1, report.Added)
	assert.Equal(t, 1, report.Failed)
	assert.Equal(t, []string{"held.txt"}, failed)
}

// A byte-range lock lets push open the file but not read it. Watch mode must
// retry once the lock is released, even though the file never changes.
func TestPushWatchUploadsAFileAfterItsByteRangeLockIsReleased(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked.txt")
	require.NoError(t, os.WriteFile(locked, []byte("locked contents"), 0o600))
	name, err := windows.UTF16PtrFromString(locked)
	require.NoError(t, err)
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	require.NoError(t, err)
	defer func() { require.NoError(t, windows.CloseHandle(handle)) }()
	var lock windows.Overlapped
	require.NoError(t, windows.LockFileEx(handle,
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1<<20, 0, &lock))
	unlocked := false
	defer func() {
		if !unlocked {
			_ = windows.UnlockFileEx(handle, 0, 1<<20, 0, &lock)
		}
	}()

	opts := options(dir)
	opts.Watch = true
	opts.Folder.SettleTime = config.Duration(100 * time.Millisecond)
	opts.Folder.ScanInterval = config.Duration(20 * time.Millisecond)
	failures := make(chan string, 4)
	opts.Failure = func(ref string, _ error) error { failures <- ref; return nil }
	events := make(chan string, 4)
	opts.Progress = func(_ string, outcome string) error { events <- outcome; return nil }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := push.Run(ctx, f.connection, opts); done <- err }()

	// Each watch scan with files to push reads the cursors before it reads any
	// file. Read 1 is at startup and read 2 precedes the first attempt on the
	// locked file. A third read can only come after that attempt failed and
	// the file was observed again; a permanent rejection would never get here.
	require.Eventually(t, func() bool { return f.listings.Load() >= 3 }, 30*time.Second, 20*time.Millisecond,
		"the locked file is retried rather than rejected")
	assert.Empty(t, failures, "a locked file is retried, not reported")
	assert.Empty(t, events, "nothing is pushed while the file is locked")

	require.NoError(t, windows.UnlockFileEx(handle, 0, 1<<20, 0, &lock))
	unlocked = true
	require.Eventually(t, func() bool { return len(events) == 1 }, 30*time.Second, 20*time.Millisecond)
	assert.Equal(t, "added", <-events)
	assert.Empty(t, failures)
	node, err := f.store.NodeByPath(t.Context(), "/archive/locked.txt")
	require.NoError(t, err)
	assert.Equal(t, "locked contents", readNode(t, f, node.ID))
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}
