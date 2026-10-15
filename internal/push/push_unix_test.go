//go:build unix

package push_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/push"
)

// lockedFolder creates a readable file, an unreadable file, and an unreadable
// subdirectory. Permission bits do not restrict root.
func lockedFolder(t *testing.T) string {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not restrict root")
	}
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("locked"), 0o000))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("readable"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sealed"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sealed", "c.txt"), []byte("inside"), 0o600))
	require.NoError(t, os.Chmod(filepath.Join(dir, "sealed"), 0o000))
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "sealed"), 0o700) })
	return dir
}

func TestPushReportsUnreadableEntriesAndContinues(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := lockedFolder(t)
	opts := options(dir)
	var failed []string
	opts.Failure = func(ref string, _ error) error { failed = append(failed, ref); return nil }
	report, err := push.Run(t.Context(), f.connection, opts)
	require.ErrorIs(t, err, push.ErrFilesFailed)
	assert.Equal(t, 1, report.Added)
	assert.Equal(t, 2, report.Failed)
	assert.Equal(t, []string{"a.txt", "sealed"}, failed)
}

func TestPushWatchReportsAnUnreadableFileOnceUntilItIsReadable(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := lockedFolder(t)
	opts := options(dir)
	opts.Watch = true
	opts.Folder.SettleTime = config.Duration(100 * time.Millisecond)
	opts.Folder.ScanInterval = config.Duration(20 * time.Millisecond)
	failures := make(chan string, 16)
	opts.Failure = func(ref string, _ error) error { failures <- ref; return nil }
	events := make(chan string, 8)
	opts.Progress = func(_ string, outcome string) error { events <- outcome; return nil }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := push.Run(ctx, f.connection, opts); done <- err }()
	require.Eventually(t, func() bool { return len(events) == 1 }, 30*time.Second, 20*time.Millisecond)
	// A new file needs a full settle window, so many scans pass before it is pushed.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "d.txt"), []byte("later"), 0o600))
	require.Eventually(t, func() bool { return len(events) == 2 }, 30*time.Second, 20*time.Millisecond)
	assert.Len(t, failures, 2, "each unreadable entry is reported once across many scans")

	require.NoError(t, os.Chmod(filepath.Join(dir, "a.txt"), 0o600))
	require.Eventually(t, func() bool { return len(events) == 3 }, 30*time.Second, 20*time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

// A file that becomes unreadable while it uploads cannot be rechecked
// afterwards. Push reports it and still pushes the files after it.
func TestPushReportsAFileThatBecomesUnreadableDuringUpload(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not restrict root")
	}
	f := newFixture(t)
	dir := t.TempDir()
	revoked := filepath.Join(dir, "a.txt")
	require.NoError(t, os.WriteFile(revoked, []byte("revoked"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("readable"), 0o600))
	revoke := func() { assert.NoError(t, os.Chmod(revoked, 0o000)) }
	f.onUpload.Store(&revoke)

	opts := options(dir)
	var failed []string
	opts.Failure = func(ref string, err error) error {
		failed = append(failed, ref)
		assert.ErrorIs(t, err, os.ErrPermission)
		return nil
	}
	report, err := push.Run(t.Context(), f.connection, opts)
	require.ErrorIs(t, err, push.ErrFilesFailed)
	assert.Equal(t, []string{"a.txt"}, failed)
	assert.Equal(t, 1, report.Failed)
	b, err := f.store.NodeByPath(t.Context(), "/archive/b.txt")
	require.NoError(t, err, "files after the unreadable one are still pushed")
	assert.Equal(t, "readable", readNode(t, f, b.ID))
}
