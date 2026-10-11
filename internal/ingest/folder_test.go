package ingest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/config"
)

func TestFolderWatchSettlesExcludesAndReobservesAfterRestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "cache"), 0700))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "nested"), 0700))
	for _, ref := range []string{"notes.txt", "nested/notes.txt", "cache/ignored.txt", "nested/.DS_Store"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, ref), []byte("stable"), 0600))
	}
	cfg := config.WatchConfig{Name: "laptop", Source: dir, Destination: "/archive", SettleTime: config.Duration(30 * time.Second), ScanInterval: config.Duration(5 * time.Second), Exclude: []string{"cache/", ".DS_Store"}}
	var observed []string
	processor := func(_ context.Context, file FolderFile) error { observed = append(observed, file.Ref); return nil }
	scanner, err := NewFolderScanner(cfg, processor)
	require.NoError(t, err)
	now := time.Now()
	scanner.watcher.now = func() time.Time { return now }
	root, err := scanner.openRoot()
	require.NoError(t, err)
	defer func() { require.NoError(t, root.Close()) }()
	require.NoError(t, scanner.watcher.scan(t.Context(), root))
	assert.Empty(t, observed)
	now = now.Add(29 * time.Second)
	require.NoError(t, scanner.watcher.scan(t.Context(), root))
	assert.Empty(t, observed)
	now = now.Add(time.Second)
	require.NoError(t, scanner.watcher.scan(t.Context(), root))
	assert.Equal(t, []string{"nested/notes.txt", "notes.txt"}, observed)
	require.NoError(t, scanner.watcher.scan(t.Context(), root))
	assert.Len(t, observed, 2)
	restarted, err := NewFolderScanner(cfg, processor)
	require.NoError(t, err)
	restarted.watcher.now = func() time.Time { return now }
	restarted.watcher.sourceMount = scanner.watcher.sourceMount
	require.NoError(t, restarted.watcher.scan(t.Context(), root))
	assert.Len(t, observed, 2, "restart must prove a full settle window")
	now = now.Add(30 * time.Second)
	require.NoError(t, restarted.watcher.scan(t.Context(), root))
	assert.Len(t, observed, 4)
}

func TestFolderScanSkipsFileThatDisappearsBeforeObservation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"a-disappears.txt", "b-stable.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("synthetic"), 0600))
	}
	var observed []string
	scanner, err := NewFolderScanner(config.WatchConfig{Name: "laptop", Source: dir,
		Destination: "/archive", SettleTime: config.Duration(time.Second),
		ScanInterval: config.Duration(time.Second)},
		func(_ context.Context, file FolderFile) error {
			observed = append(observed, file.Ref)
			return nil
		})
	require.NoError(t, err)
	scanner.watcher.beforeObserve = func(ref string) {
		if ref == "a-disappears.txt" {
			require.NoError(t, os.Remove(filepath.Join(dir, ref)))
		}
	}

	err = scanner.ScanOnce(t.Context())
	require.NoError(t, err, "a transiently missing entry must not abort the one-shot scan")
	assert.Equal(t, []string{"b-stable.txt"}, observed)
}

func TestFolderMinimumAgeAndSourceChanges(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(file, []byte("first"), 0600))
	now := time.Now().Truncate(time.Second)
	require.NoError(t, os.Chtimes(file, now, now))
	cfg := config.WatchConfig{Name: "laptop", Source: dir, Destination: "/archive", SettleTime: config.Duration(30 * time.Second), MinimumAge: config.Duration(time.Hour), ScanInterval: config.Duration(5 * time.Second)}
	count := 0
	scanner, err := NewFolderScanner(cfg, func(_ context.Context, _ FolderFile) error { count++; return nil })
	require.NoError(t, err)
	scanner.watcher.now = func() time.Time { return now }
	root, err := scanner.openRoot()
	require.NoError(t, err)
	defer func() { require.NoError(t, root.Close()) }()
	require.NoError(t, scanner.watcher.scan(t.Context(), root))
	now = now.Add(30 * time.Second)
	require.NoError(t, scanner.watcher.scan(t.Context(), root))
	assert.Zero(t, count)
	now = now.Add(time.Hour)
	require.NoError(t, scanner.watcher.scan(t.Context(), root))
	assert.Equal(t, 1, count)
	require.NoError(t, os.WriteFile(file, []byte("new bytes"), 0600))
	require.NoError(t, os.Chtimes(file, now, now))
	require.NoError(t, scanner.watcher.scan(t.Context(), root))
	now = now.Add(time.Hour)
	require.NoError(t, scanner.watcher.scan(t.Context(), root))
	assert.Equal(t, 2, count)
}

func TestFolderWatchRestartsSettleAfterProcessorFindsChangedSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("before upload"), 0600))
	cfg := config.WatchConfig{Name: "laptop", Source: dir, Destination: "/archive",
		SettleTime: config.Duration(10 * time.Second), ScanInterval: config.Duration(time.Second)}
	now := time.Now()
	calls := 0
	scanner, err := NewFolderScanner(cfg, func(_ context.Context, _ FolderFile) error {
		calls++
		if calls == 1 {
			require.NoError(t, os.WriteFile(filePath, []byte("changed during upload"), 0600))
			return errors.New("remote digest_mismatch")
		}
		return nil
	})
	require.NoError(t, err)
	scanner.watcher.now = func() time.Time { return now }
	root, err := scanner.openRoot()
	require.NoError(t, err)
	defer func() { require.NoError(t, root.Close()) }()

	require.NoError(t, scanner.watcher.scan(t.Context(), root))
	assert.Zero(t, calls, "the first observation must settle before upload")
	now = now.Add(10 * time.Second)
	err = scanner.watcher.scan(t.Context(), root)
	require.NoError(t, err,
		"a remote digest error caused by source mutation should restart settling")
	assert.Equal(t, 1, calls)
	require.NoError(t, scanner.watcher.scan(t.Context(), root),
		"the changed source should begin a fresh settle window")
	now = now.Add(9 * time.Second)
	require.NoError(t, scanner.watcher.scan(t.Context(), root))
	assert.Equal(t, 1, calls, "changed bytes must settle again before retry")
	now = now.Add(time.Second)
	require.NoError(t, scanner.watcher.scan(t.Context(), root))
	assert.Equal(t, 2, calls)
}

func TestFolderWatchPreservesProcessorErrorWhenSourceIsUnchanged(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("stable"), 0600))
	remoteErr := errors.New("remote unavailable")
	cfg := config.WatchConfig{Name: "laptop", Source: dir, Destination: "/archive",
		SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Second)}
	now := time.Now()
	scanner, err := NewFolderScanner(cfg, func(_ context.Context, _ FolderFile) error { return remoteErr })
	require.NoError(t, err)
	scanner.watcher.now = func() time.Time { return now }
	root, err := scanner.openRoot()
	require.NoError(t, err)
	defer func() { require.NoError(t, root.Close()) }()
	require.NoError(t, scanner.watcher.scan(t.Context(), root))
	now = now.Add(time.Second)
	err = scanner.watcher.scan(t.Context(), root)
	require.ErrorIs(t, err, remoteErr)
}
