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
	scanner, err := NewFolderScanner(cfg, processor, failOnReject(t))
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
	restarted, err := NewFolderScanner(cfg, processor, failOnReject(t))
	require.NoError(t, err)
	restarted.watcher.now = func() time.Time { return now }
	restarted.watcher.sourceMount = scanner.watcher.sourceMount
	require.NoError(t, restarted.watcher.scan(t.Context(), root))
	assert.Len(t, observed, 2, "restart must prove a full settle window")
	now = now.Add(30 * time.Second)
	require.NoError(t, restarted.watcher.scan(t.Context(), root))
	assert.Len(t, observed, 4)
}

func TestFolderWatchRechecksDigestsAndSettlesContentChanges(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		changeModTime bool
		retry         bool
	}{
		{name: "content edit"},
		{name: "metadata edit", changeModTime: true},
		{name: "retryable failure", retry: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "notes.txt")
			require.NoError(t, os.WriteFile(file, []byte("first"), 0o600))
			before, err := os.Stat(file)
			require.NoError(t, err)
			var digests []string
			scanner, err := NewFolderScanner(config.WatchConfig{
				Name: "laptop", Source: dir, Destination: "/archive",
				SettleTime: config.Duration(10 * time.Second), ScanInterval: config.Duration(time.Second),
			}, func(ctx context.Context, file FolderFile) error {
				digest, err := file.Digest(ctx)
				if err == nil {
					digests = append(digests, digest)
					if test.retry && len(digests) == 1 {
						return ErrRetryLater
					}
				}
				return err
			}, failOnReject(t))
			require.NoError(t, err)
			now := time.Now()
			scanner.watcher.now = func() time.Time { return now }
			root, err := scanner.openRoot()
			require.NoError(t, err)
			defer func() { require.NoError(t, root.Close()) }()
			require.NoError(t, scanner.watcher.scan(t.Context(), root))
			now = now.Add(10 * time.Second)
			require.NoError(t, scanner.watcher.scan(t.Context(), root))
			require.Len(t, digests, 1)
			require.NoError(t, scanner.watcher.scan(t.Context(), root))
			require.Len(t, digests, 1, "unchanged bytes do not repeat processing")
			modTime := before.ModTime()
			if test.changeModTime {
				modTime = modTime.Add(time.Minute)
			}
			require.NoError(t, os.WriteFile(file, []byte("other"), 0o600))
			require.NoError(t, os.Chtimes(file, modTime, modTime))
			require.NoError(t, scanner.watcher.scan(t.Context(), root))
			require.Len(t, digests, 1, "a digest change must settle before processing")
			now = now.Add(9 * time.Second)
			require.NoError(t, os.WriteFile(file, []byte("third"), 0o600))
			require.NoError(t, os.Chtimes(file, modTime, modTime))
			require.NoError(t, scanner.watcher.scan(t.Context(), root))
			require.Len(t, digests, 1)
			now = now.Add(time.Second)
			require.NoError(t, scanner.watcher.scan(t.Context(), root))
			require.Len(t, digests, 1, "the second digest change must restart the settle window")
			now = now.Add(8 * time.Second)
			require.NoError(t, scanner.watcher.scan(t.Context(), root))
			require.Len(t, digests, 1)
			now = now.Add(time.Second)
			require.NoError(t, scanner.watcher.scan(t.Context(), root))
			require.Len(t, digests, 2)
			assert.NotEqual(t, digests[0], digests[1])
			require.NoError(t, scanner.watcher.scan(t.Context(), root))
			assert.Len(t, digests, 2, "the new bytes also remain quiet once processed")
		})
	}
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
		}, failOnReject(t))
	require.NoError(t, err)
	scanner.watcher.beforeObserve = func(ref string) {
		if ref == "a-disappears.txt" {
			require.NoError(t, os.Remove(filepath.Join(dir, ref)))
		}
	}

	err = scanner.ScanOnce(t.Context(), 1)
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
	scanner, err := NewFolderScanner(cfg, func(_ context.Context, _ FolderFile) error { count++; return nil }, failOnReject(t))
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
	}, failOnReject(t))
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
	scanner, err := NewFolderScanner(cfg, func(_ context.Context, _ FolderFile) error { return remoteErr }, failOnReject(t))
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

func TestFolderWatchReportsPersistentReadFailuresOnceUntilRecovery(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(path, []byte("first"), 0o600))
	failReads := true
	attempts, processed, rejected := 0, 0, 0
	scanner, err := NewFolderScanner(config.WatchConfig{
		Name: "laptop", Source: dir, Destination: "/archive",
		SettleTime: config.Duration(10 * time.Second), ScanInterval: config.Duration(time.Second),
	}, func(ctx context.Context, file FolderFile) error {
		attempts++
		if failReads {
			require.NoError(t, file.File.Close())
		}
		_, err := file.Digest(ctx)
		if err == nil {
			processed++
		}
		return err
	}, func(_ context.Context, ref string, err error) error {
		require.Equal(t, "notes.txt", ref)
		require.ErrorIs(t, err, os.ErrClosed)
		rejected++
		return nil
	})
	require.NoError(t, err)
	scanner.watcher.rejected = make(map[string]struct{})
	now := time.Now()
	scanner.watcher.now = func() time.Time { return now }
	root, err := scanner.openRoot()
	require.NoError(t, err)
	defer func() { require.NoError(t, root.Close()) }()
	scan := func() {
		t.Helper()
		require.NoError(t, scanner.watcher.scan(t.Context(), root))
		now = now.Add(10 * time.Second)
	}
	for range 6 {
		scan()
	}
	require.Equal(t, 3, attempts, "failed reads must be retried to detect recovery")
	require.Equal(t, 1, rejected, "the same persistent read failure is reported only once")

	failReads = false
	scan()
	scan()
	require.Equal(t, 1, processed, "readable bytes must eventually be processed")
	failReads = true
	require.NoError(t, os.WriteFile(path, []byte("changed contents"), 0o600))
	scan()
	scan()
	require.Equal(t, 2, rejected, "a failure after recovery must be reported again")

	require.NoError(t, os.Remove(path))
	scan()
	require.NoError(t, os.WriteFile(path, []byte("recreated"), 0o600))
	scan()
	scan()
	require.Equal(t, 3, rejected, "a new entry must not inherit the disappeared entry's rejection")
}

func failOnReject(t *testing.T) func(context.Context, string, error) error {
	t.Helper()
	return func(_ context.Context, ref string, err error) error {
		t.Errorf("unexpected rejection of %s: %v", ref, err)
		return nil
	}
}

func TestFolderScannerScanOnceSourceChangePolicy(t *testing.T) {
	t.Parallel()
	for _, continueChanged := range []bool{false, true} {
		name := "abort"
		if continueChanged {
			name = "continue"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("unstable"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "z.txt"), []byte("stable"), 0o600))
			var processed []string
			scanner, err := NewFolderScanner(config.WatchConfig{Name: "checkout-test", Source: dir, Destination: "/", SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Second)}, func(_ context.Context, file FolderFile) error {
				if file.Ref == "a.txt" {
					return ErrSourceChanged
				}
				processed = append(processed, file.Ref)
				return nil
			}, failOnReject(t))
			require.NoError(t, err)
			var changed []string
			if continueChanged {
				scanner.SourceChanged = func(_ context.Context, ref string) error { changed = append(changed, ref); return nil }
			}
			err = scanner.ScanOnce(t.Context(), 1)
			if continueChanged {
				require.NoError(t, err)
				require.Equal(t, []string{"a.txt"}, changed)
				require.Equal(t, []string{"z.txt"}, processed)
			} else {
				require.ErrorIs(t, err, ErrSourceChanged)
				require.Empty(t, processed)
			}
		})
	}
}

func TestFolderScannerExcludePatterns(t *testing.T) {
	source := writeTree(t, map[string]string{"note.md": "real", "nested/x.swp": "swap", "cache-1/data.bin": "cached"})
	var observed []string
	scanner, err := NewFolderScanner(config.WatchConfig{Name: "patterns", Source: source, Destination: "/archive", SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Second), ExcludePatterns: []string{"*.swp", "cache-*"}}, func(_ context.Context, file FolderFile) error { observed = append(observed, file.Ref); return nil }, failOnReject(t))
	require.NoError(t, err)
	require.NoError(t, scanner.ScanOnce(t.Context(), 1))
	assert.Equal(t, []string{"note.md"}, observed)
}

func TestFolderWatchHintDoesNotHashUnrelatedAcceptedFile(t *testing.T) {
	source := t.TempDir()
	for _, ref := range []string{"one.txt", "two.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(source, ref), []byte(ref), 0600))
	}
	processed := make(map[string]int)
	scanner, err := NewFolderScanner(config.WatchConfig{
		Name: "notes", Source: source, Destination: "/archive", CaptureOnChange: true,
		SettleTime: config.Duration(time.Hour), ScanInterval: config.Duration(time.Hour),
	}, func(ctx context.Context, file FolderFile) error {
		processed[file.Ref]++
		_, err := file.Digest(ctx)
		return err
	}, failOnReject(t))
	require.NoError(t, err)
	root, err := scanner.openRoot()
	require.NoError(t, err)
	defer func() { require.NoError(t, root.Close()) }()
	require.NoError(t, scanner.watcher.scan(t.Context(), root))
	require.Equal(t, map[string]int{"one.txt": 1, "two.txt": 1}, processed)
	twoPath := filepath.Join(source, "two.txt")
	twoInfo, err := os.Stat(twoPath)
	require.NoError(t, err)
	// Preserve the file fingerprint so only an unfiltered digest recheck can
	// discover this edit during the hinted scan.
	require.NoError(t, os.WriteFile(twoPath, []byte("TWO.TXT"), 0600))
	require.NoError(t, os.Chtimes(twoPath, twoInfo.ModTime(), twoInfo.ModTime()))
	twoAfter, err := os.Stat(twoPath)
	require.NoError(t, err)
	require.Equal(t, twoInfo.Size(), twoAfter.Size())
	assert.True(t, twoInfo.ModTime().Equal(twoAfter.ModTime()))
	requireScanComplete(t, checkedScan(scanner.watcher.scanWithHints(t.Context(), root, watchScanHints{
		Dirty: map[string]struct{}{"one.txt": {}},
	})))
	require.Equal(t, map[string]int{"one.txt": 2, "two.txt": 1}, processed)
}
