//go:build !windows

package ingest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/config"
)

func TestFolderScanNeverFollowsSymlinksAndRejectsRootReplacement(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "private.txt"), []byte("synthetic outside bytes"), 0600))
	require.NoError(t, os.Symlink(filepath.Join(outside, "private.txt"), filepath.Join(dir, "file-link")))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "directory-link")))
	count := 0
	scanner, err := NewFolderScanner(config.WatchConfig{Name: "laptop", Source: dir, Destination: "/archive", SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Second)}, func(_ context.Context, _ FolderFile) error { count++; return nil })
	require.NoError(t, err)
	require.NoError(t, scanner.ScanOnce(t.Context()))
	assert.Zero(t, count)
	root, err := scanner.openRoot()
	require.NoError(t, err)
	defer func() { require.NoError(t, root.Close()) }()
	require.NoError(t, os.Rename(dir, dir+"-old"))
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir+"-old")) })
	require.NoError(t, os.Mkdir(dir, 0700))
	require.ErrorIs(t, scanner.watcher.scan(t.Context(), root), ErrSourceChanged)
}

func TestFolderScanReportsRootReplacementDuringOneShotScan(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"a-first.txt", "b-second.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("synthetic"), 0600))
	}
	oldRoot := dir + "-old"
	processed := 0
	scanner, err := NewFolderScanner(config.WatchConfig{Name: "laptop", Source: dir, Destination: "/archive",
		SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Second)},
		func(_ context.Context, _ FolderFile) error {
			processed++
			if processed == 1 {
				if err := os.Rename(dir, oldRoot); err != nil {
					return err
				}
				return os.Mkdir(dir, 0700)
			}
			return nil
		})
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(oldRoot) })

	err = scanner.ScanOnce(t.Context())
	require.ErrorIs(t, err, ErrSourceChanged)
	assert.Equal(t, 1, processed, "one-shot scan must stop when its pinned root is replaced")
}
