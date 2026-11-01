//go:build darwin

package ingest

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openDescriptorCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	require.NoError(t, err)
	return len(entries)
}

// kqueue keeps one descriptor per file in a watched directory. When that
// directory is renamed, fsnotify drops only the directory's own watch, so the
// notifier must replace its backend to release the files' descriptors.
func TestKqueueRenamedDirectoryReleasesFileWatches(t *testing.T) {
	const files = 20
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "before"), 0o700))
	for i := range files {
		require.NoError(t, os.WriteFile(filepath.Join(root, "before", fmt.Sprintf("file-%d.txt", i)), []byte("x"), 0o600))
	}
	n, err := newWatchNotifications(root, exclusions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, n.Close()) })
	require.NotNil(t, n.replaceBackend, "darwin uses the kqueue backend")
	enroll := func(dir string) {
		t.Helper()
		n.BeginScan()
		require.NoError(t, n.AddDirectory("", 1))
		require.NoError(t, n.AddDirectory(dir, files))
		require.NoError(t, n.EndScan())
	}
	enroll("before")
	baseline := openDescriptorCount(t)

	names := []string{"before", "after"}
	for i := range 6 {
		from, to := filepath.Join(root, names[i%2]), filepath.Join(root, names[(i+1)%2])
		require.NoError(t, os.Rename(from, to))
		// Wait until kqueue reports the rename and fsnotify drops the old path.
		require.Eventually(t, func() bool {
			backend, ok := n.backend.(fsnotifyBackend)
			return ok && !slices.Contains(backend.watcher.WatchList(), from)
		}, 10*time.Second, 10*time.Millisecond)
		_ = n.Drain()
		enroll(names[(i+1)%2])
	}
	assert.LessOrEqual(t, openDescriptorCount(t), baseline+2,
		"each rename must not leave its %d file watches open", files)
}
