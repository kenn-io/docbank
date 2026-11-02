//go:build unix

package backup

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestReadRegularFileRejectsFifoWithoutOpening pins the pre-open type check:
// opening a fifo blocks until a writer appears, so a fifo planted at a
// referenced attachment path must be rejected by the stat that runs BEFORE
// the open — capture must fail loudly, not hang uninterruptibly. The read
// runs in a goroutine so a regression blocks only it, not the whole suite.
func TestReadRegularFileRejectsFifoWithoutOpening(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	require.NoError(syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o600))
	root, err := os.OpenRoot(dir)
	require.NoError(err)
	defer func() { _ = root.Close() }()

	done := make(chan error, 1)
	go func() {
		_, _, err := readRegularFile(root, "fifo")
		done <- err
	}()
	select {
	case err := <-done:
		require.ErrorContains(err, "not a regular file")
	case <-time.After(5 * time.Second):
		require.Fail("readRegularFile blocked opening a fifo instead of rejecting it")
	}
}

// TestReadRegularFileFollowsInRootSymlink pins that the pre-open check uses
// stat, not lstat: an in-root symlink to a regular file stays capturable —
// attachment bytes are hash-verified after reading, unlike extras, so
// following in-root links is safe and rejecting them would break existing
// content trees that use them.
func TestReadRegularFileFollowsInRootSymlink(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(dir, "real"), []byte("bytes"), 0o600))
	require.NoError(os.Symlink("real", filepath.Join(dir, "link")))
	root, err := os.OpenRoot(dir)
	require.NoError(err)
	defer func() { _ = root.Close() }()

	content, _, err := readRegularFile(root, "link")
	require.NoError(err)
	require.Equal([]byte("bytes"), content)
}
