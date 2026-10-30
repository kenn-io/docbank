package ingest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
)

type scanResult struct {
	complete bool
	err      error
}

func checkedScan(complete bool, err error) scanResult { return scanResult{complete, err} }
func requireScanComplete(t *testing.T, result scanResult) {
	t.Helper()
	require.NoError(t, result.err)
	require.True(t, result.complete)
}
func requireScanErrorIs(t *testing.T, result scanResult, target error) {
	t.Helper()
	require.False(t, result.complete)
	require.ErrorIs(t, result.err, target)
}

func openWatcherRoot(t *testing.T, watcher *Watcher) *watchRoot {
	t.Helper()
	root, err := watcher.openRoot(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })
	return root
}

func renameWatcherRoot(t *testing.T, root *watchRoot, moved string) {
	t.Helper()
	// Windows prevents renaming an open os.Root. Reopen the moved directory
	// while retaining the original pinned path and identity for verification.
	if runtime.GOOS == "windows" {
		require.NoError(t, root.Close())
	}
	require.NoError(t, os.Rename(root.path, moved))
	if runtime.GOOS == "windows" {
		reopened, err := os.OpenRoot(moved)
		require.NoError(t, err)
		root.root = reopened
	}
	info, err := root.root.Stat(".")
	require.NoError(t, err)
	require.True(t, os.SameFile(root.identity, info))
}

func scanWatcherAt(ctx context.Context, watcher *Watcher, root *watchRoot, at time.Time) error {
	previous := watcher.now
	watcher.now = func() time.Time { return at }
	defer func() { watcher.now = previous }()
	return watcher.scan(ctx, root)
}

func TestWatcherReportsBusyOnlyWhenIngesting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ing := newTestIngester(t)
		processed := 0
		source := writeTree(t, map[string]string{"report.txt": "report"})
		var supervisor *jobs.Supervisor
		watcher, err := NewWatcher(ing, t.TempDir(), config.WatchConfig{
			Name: "activity", Source: source, Destination: "/archive",
			SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Second),
		}, func(fn func() error) error {
			assert.True(t, supervisor.Snapshot()[0].Busy, "ingesting a settled file is busy")
			err := fn()
			if err == nil {
				processed++
			}
			return err
		}, slog.New(slog.DiscardHandler))
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		supervisor = jobs.New(ctx, slog.New(slog.DiscardHandler))
		t.Cleanup(func() { require.NoError(t, supervisor.Shutdown(context.Background())) })
		observedAfterIngest := false
		watcher.now = func() time.Time {
			assert.False(t, supervisor.Snapshot()[0].Busy, "observing files is idle")
			if processed > 0 {
				_, err := ing.Store.NodeByPath(ctx, "/archive/report.txt")
				require.NoError(t, err)
				observedAfterIngest = true
			}
			return time.Now()
		}
		require.NoError(t, supervisor.Start("watch:activity", watcher.Run))
		synctest.Wait()
		require.NoError(t, os.WriteFile(filepath.Join(source, "z-unsettled.txt"), []byte("still writing"), 0o600))
		time.Sleep(time.Second)
		synctest.Wait()
		assert.Equal(t, 1, processed)
		// A scan collects its inventory before importing, so the next scan
		// observes after the import. Keep the second file unsettled for it.
		require.NoError(t, os.WriteFile(filepath.Join(source, "z-unsettled.txt"), []byte("still writing more"), 0o600))
		time.Sleep(time.Second)
		synctest.Wait()
		assert.Equal(t, 1, processed)
		assert.True(t, observedAfterIngest, "scan inspected an unsettled file after the import committed")
		assert.False(t, supervisor.Snapshot()[0].Busy, "the scan finished")
		_, err = ing.Store.NodeByPath(ctx, "/archive/z-unsettled.txt")
		require.ErrorIs(t, err, store.ErrNotFound)
	})
}

func FuzzWatcherSameSizeAndMtimeContentVersions(f *testing.F) {
	f.Add([]byte("first\n"), byte(1))
	f.Add([]byte("first\n"), byte(0))
	f.Fuzz(func(t *testing.T, initial []byte, mask byte) {
		if len(initial) == 0 || len(initial) > 256 {
			t.Skip()
		}
		updated := bytes.Clone(initial)
		updated[len(updated)-1] ^= mask
		assertWatcherSameSizeAndMtimeVersions(t, initial, updated)
	})
}

// A size-and-mtime fingerprint alone leaves this ordinary edit invisible.
func assertWatcherSameSizeAndMtimeVersions(t *testing.T, initial, changed []byte) {
	t.Helper()
	wantVersions := 1
	if !bytes.Equal(initial, changed) {
		wantVersions = 2
	}
	ing := newTestIngester(t)
	mutationCalls := 0
	mutate := func(fn func() error) error {
		mutationCalls++
		return fn()
	}
	source := writeTree(t, map[string]string{"notes.txt": string(initial)})
	watcher, err := NewWatcher(ing, t.TempDir(), config.WatchConfig{
		Name: "notes", Source: source, Destination: "/archive",
		SettleTime: config.Duration(10 * time.Second), ScanInterval: config.Duration(time.Second),
	}, mutate, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	root := openWatcherRoot(t, watcher)
	t0 := time.Unix(1_700_000_000, 0)
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, t0))
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, t0.Add(10*time.Second)))
	accepted, err := ing.Store.NodeByPath(t.Context(), "/archive/notes.txt")
	require.NoError(t, err)
	assert.Equal(t, 1, mutationCalls)

	for _, seconds := range []int{11, 12, 13} {
		require.NoError(t, scanWatcherAt(t.Context(), watcher, root, t0.Add(time.Duration(seconds)*time.Second)))
	}
	assert.Equal(t, 1, mutationCalls)

	sourcePath := filepath.Join(source, "notes.txt")
	rewriteSourceWithRestoredMtime(t, sourcePath, changed)

	for _, seconds := range []int{20, 29} {
		require.NoError(t, scanWatcherAt(t.Context(), watcher, root, t0.Add(time.Duration(seconds)*time.Second)))
		node, err := ing.Store.NodeByID(t.Context(), accepted.ID)
		require.NoError(t, err)
		assert.Equal(t, accepted, node)
		_, total, err := ing.Store.ContentVersions(t.Context(), accepted.ID, 10, 0)
		require.NoError(t, err)
		assert.Equal(t, 1, total)
		assert.Equal(t, 1, mutationCalls)
	}
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, t0.Add(30*time.Second)))
	updated, err := ing.Store.NodeByPath(t.Context(), "/archive/notes.txt")
	require.NoError(t, err)
	assert.Equal(t, accepted.ID, updated.ID)
	if wantVersions == 2 {
		assert.NotEqual(t, accepted.CurrentVersionID, updated.CurrentVersionID)
	} else {
		assert.Equal(t, accepted.CurrentVersionID, updated.CurrentVersionID)
	}
	content, err := ing.Blobs.Open(updated.BlobHash)
	require.NoError(t, err)
	currentBytes, err := io.ReadAll(content)
	require.NoError(t, err)
	require.NoError(t, content.Close())
	assert.Equal(t, changed, currentBytes)
	for _, seconds := range []int{40, 50} {
		require.NoError(t, scanWatcherAt(t.Context(), watcher, root, t0.Add(time.Duration(seconds)*time.Second)))
		node, err := ing.Store.NodeByID(t.Context(), accepted.ID)
		require.NoError(t, err)
		assert.Equal(t, updated, node)
	}
	versions, total, err := ing.Store.ContentVersions(t.Context(), accepted.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, wantVersions, total)
	assert.Equal(t, 2, mutationCalls)
	require.Len(t, versions, wantVersions)
	for _, version := range versions {
		content, err := ing.Blobs.Open(version.BlobHash)
		require.NoError(t, err)
		got, err := io.ReadAll(content)
		require.NoError(t, err)
		require.NoError(t, content.Close())
		if version.ID == accepted.CurrentVersionID {
			assert.Equal(t, initial, got)
		} else {
			assert.Equal(t, changed, got)
		}
	}
}

// Rewrite the file with the same size and restored mtime, retrying until native change time moves.
func rewriteSourceWithRestoredMtime(t *testing.T, path string, content []byte) {
	t.Helper()
	file, err := os.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, file.Close()) }()
	before, err := file.Stat()
	require.NoError(t, err)
	require.Equal(t, before.Size(), int64(len(content)))
	stamp := fileChangeTime(file, before)
	if stamp == 0 {
		t.Skip("test filesystem does not report native change time")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		require.NoError(t, os.WriteFile(path, content, 0o600))
		require.NoError(t, os.Chtimes(path, before.ModTime(), before.ModTime()))
		after, err := file.Stat()
		require.NoError(t, err)
		if fileChangeTime(file, after) != stamp {
			return
		}
		require.True(t, time.Now().Before(deadline), "filesystem change time did not advance")
		time.Sleep(10 * time.Millisecond) //nolint:kennlint // sleeptest: wait for the native filesystem clock
	}
}

func TestWatcherWaitsForStabilityAndUpdatesTheSameMovedNode(t *testing.T) {
	ing := newTestIngester(t)
	expectedUpdatedContent := []byte("{\"message\":\"second\"}\n")
	mutationCalls := 0
	mutate := func(fn func() error) error {
		mutationCalls++
		return fn()
	}
	source := writeTree(t, map[string]string{
		"sub/session.jsonl": "{\"message\":\"first\"}\n",
		"ignored.tmp":       "partial",
	})
	watcher, err := NewWatcher(ing, t.TempDir(), config.WatchConfig{
		Name: "sessions", Source: source, Destination: "/archive",
		SettleTime: config.Duration(10 * time.Second), ScanInterval: config.Duration(time.Second),
		Exclude: []string{"ignored.tmp"},
	}, mutate, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	root := openWatcherRoot(t, watcher)
	t0 := time.Unix(1_700_000_000, 0)

	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, t0))
	_, err = ing.Store.NodeByPath(t.Context(), "/archive/sub/session.jsonl")
	require.ErrorIs(t, err, store.ErrNotFound)
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, t0.Add(9*time.Second)))
	_, err = ing.Store.NodeByPath(t.Context(), "/archive/sub/session.jsonl")
	require.ErrorIs(t, err, store.ErrNotFound)

	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, t0.Add(10*time.Second)))
	assert.Equal(t, 1, mutationCalls)
	node, err := ing.Store.NodeByPath(t.Context(), "/archive/sub/session.jsonl")
	require.NoError(t, err)
	assert.Equal(t, int64(1), node.Revision)
	_, err = ing.Store.NodeByPath(t.Context(), "/archive/ignored.tmp")
	require.ErrorIs(t, err, store.ErrNotFound)

	movedParent, err := ing.Store.MkdirAll(t.Context(), "/organized")
	require.NoError(t, err)
	moved, movedPath, err := ing.Store.Move(
		t.Context(), node.ID, movedParent.ID, "renamed.jsonl", node.Revision,
	)
	require.NoError(t, err)
	assert.Equal(t, "/organized/renamed.jsonl", movedPath)

	sourcePath := filepath.Join(source, "sub", "session.jsonl")
	require.NoError(t, os.WriteFile(sourcePath, []byte("{\"message\":\"second\"}\n"), 0o644))
	changedAt := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(sourcePath, changedAt, changedAt))
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, t0.Add(20*time.Second)))
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, t0.Add(30*time.Second)))
	assert.Equal(t, 2, mutationCalls)

	updated, err := ing.Store.NodeByID(t.Context(), node.ID)
	require.NoError(t, err)
	assert.Equal(t, moved.ID, updated.ID)
	assert.Equal(t, "renamed.jsonl", updated.Name)
	assert.Equal(t, moved.Revision+1, updated.Revision)
	versions, total, err := ing.Store.ContentVersions(t.Context(), node.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, total)
	require.Len(t, versions, 2)
	content, err := ing.Blobs.Open(updated.BlobHash)
	require.NoError(t, err)
	contentBytes, err := io.ReadAll(content)
	require.NoError(t, err)
	require.NoError(t, content.Close())
	assert.Equal(t, expectedUpdatedContent, contentBytes)

	manualContent := "{\"message\":\"local edit\"}\n"
	manualHash, manualSize, err := ing.Blobs.Write(strings.NewReader(manualContent))
	require.NoError(t, err)
	manuallyEdited, _, err := ing.Store.ReplaceContent(
		t.Context(), node.ID, updated.Revision, manualHash, manualSize, "application/json",
	)
	require.NoError(t, err)

	// A daemon restart deliberately forgets observations and makes the same
	// unchanged source prove a full settle window again. Its durable source
	// cursor must preserve the independent edit instead of reinstalling source
	// bytes as another version.
	restarted, err := NewWatcher(
		ing, t.TempDir(), watcher.config, runTestMutation, slog.New(slog.DiscardHandler),
	)
	require.NoError(t, err)
	restartedRoot := openWatcherRoot(t, restarted)
	require.NoError(t, scanWatcherAt(t.Context(), restarted, restartedRoot, t0.Add(40*time.Second)))
	require.NoError(t, scanWatcherAt(t.Context(), restarted, restartedRoot, t0.Add(50*time.Second)))
	afterRestart, err := ing.Store.NodeByID(t.Context(), node.ID)
	require.NoError(t, err)
	assert.Equal(t, manuallyEdited, afterRestart)
	_, total, err = ing.Store.ContentVersions(t.Context(), node.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 3, total)
	sourceBytes, err := os.ReadFile(sourcePath)
	require.NoError(t, err)
	assert.Equal(t, expectedUpdatedContent, sourceBytes)
}

func TestWatcherWildcardExclusionRemainsLiteral(t *testing.T) {
	ing := newTestIngester(t)
	source := writeTree(t, map[string]string{"keep.tmp": "watch me"})
	watcher, err := NewWatcher(ing, t.TempDir(), config.WatchConfig{
		Name: "literal", Source: source, Destination: "/archive",
		SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Second),
		Exclude: []string{"*.tmp"},
	}, runTestMutation, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	root := openWatcherRoot(t, watcher)
	at := time.Unix(1_700_000_000, 0)
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, at))
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, at.Add(time.Second)))
	_, err = ing.Store.NodeByPath(t.Context(), "/archive/keep.tmp")
	require.NoError(t, err)
}

func TestWatcherDoesNotCountTraversalTimeTowardSettle(t *testing.T) {
	ing := newTestIngester(t)
	source := writeTree(t, map[string]string{
		"a.txt": "observed first",
		"z.txt": "observed last",
	})
	watcher, err := NewWatcher(ing, t.TempDir(), config.WatchConfig{
		Name: "slow-scan", Source: source, Destination: "/inbox",
		SettleTime: config.Duration(10 * time.Second), ScanInterval: config.Duration(time.Second),
	}, runTestMutation, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	root := openWatcherRoot(t, watcher)
	t0 := time.Unix(1_700_000_000, 0)

	setObservationTimes := func(times ...time.Time) {
		next := 0
		watcher.now = func() time.Time {
			require.Less(t, next, len(times))
			observedAt := times[next]
			next++
			return observedAt
		}
	}
	// Simulate a scan that reaches z.txt nine seconds after a.txt.
	setObservationTimes(t0, t0.Add(9*time.Second))
	require.NoError(t, watcher.scan(t.Context(), root))
	assert.Equal(t, t0, watcher.observations["a.txt"].stableSince)
	assert.Equal(t, t0.Add(9*time.Second), watcher.observations["z.txt"].stableSince)

	// At the next observation only a.txt has been stable for the full window.
	setObservationTimes(t0.Add(10*time.Second), t0.Add(10*time.Second))
	require.NoError(t, watcher.scan(t.Context(), root))
	_, err = ing.Store.NodeByPath(t.Context(), "/inbox/a.txt")
	require.NoError(t, err)
	_, err = ing.Store.NodeByPath(t.Context(), "/inbox/z.txt")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestWatcherRequiresMinimumSourceAgeAfterRestart(t *testing.T) {
	ing := newTestIngester(t)
	source := writeTree(t, map[string]string{
		"session.jsonl": "{\"kind\":\"closed-session\"}\n",
	})
	producedAt := time.Unix(1_700_000_000, 0)
	sourcePath := filepath.Join(source, "session.jsonl")
	require.NoError(t, os.Chtimes(sourcePath, producedAt, producedAt))
	cfg := config.WatchConfig{
		Name: "sessions", Source: source, Destination: "/archive",
		SettleTime: config.Duration(10 * time.Minute),
		MinimumAge: config.Duration(time.Hour), ScanInterval: config.Duration(time.Minute),
	}
	watcher, err := NewWatcher(
		ing, t.TempDir(), cfg, runTestMutation, slog.New(slog.DiscardHandler),
	)
	require.NoError(t, err)
	root := openWatcherRoot(t, watcher)
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, producedAt))

	// Restarting discards the first stability observation. Even after the new
	// watcher proves a complete settle window, the source is not eligible until
	// its modification time reaches the independent minimum-age boundary.
	restarted, err := NewWatcher(
		ing, t.TempDir(), cfg, runTestMutation, slog.New(slog.DiscardHandler),
	)
	require.NoError(t, err)
	restartedRoot := openWatcherRoot(t, restarted)
	require.NoError(t, scanWatcherAt(
		t.Context(), restarted, restartedRoot, producedAt.Add(20*time.Minute),
	))
	require.NoError(t, scanWatcherAt(
		t.Context(), restarted, restartedRoot, producedAt.Add(30*time.Minute),
	))
	_, err = ing.Store.NodeByPath(t.Context(), "/archive/session.jsonl")
	require.ErrorIs(t, err, store.ErrNotFound)

	require.NoError(t, scanWatcherAt(
		t.Context(), restarted, restartedRoot, producedAt.Add(time.Hour),
	))
	node, err := ing.Store.NodeByPath(t.Context(), "/archive/session.jsonl")
	require.NoError(t, err)
	assert.Equal(t, int64(1), node.Revision)
}

func TestWatcherRetriesFileThatDisappearsBeforeIngest(t *testing.T) {
	ing := newTestIngester(t)
	source := writeTree(t, map[string]string{"document.txt": "ready"})
	sourcePath := filepath.Join(source, "document.txt")
	removeBeforeIngest := false
	mutate := func(fn func() error) error {
		if removeBeforeIngest {
			removeBeforeIngest = false
			require.NoError(t, os.Remove(sourcePath))
		}
		return fn()
	}
	watcher, err := NewWatcher(ing, t.TempDir(), config.WatchConfig{
		Name: "disappearing", Source: source, Destination: "/inbox",
		SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Second),
	}, mutate, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	root := openWatcherRoot(t, watcher)
	t0 := time.Now()

	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, t0))
	assert.Len(t, watcher.observations, 1)
	removeBeforeIngest = true
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, t0.Add(time.Second)))
	assert.Empty(t, watcher.observations)
	_, err = ing.Store.NodeByPath(t.Context(), "/inbox/document.txt")
	require.ErrorIs(t, err, store.ErrNotFound)

	require.NoError(t, os.WriteFile(sourcePath, []byte("ready"), 0o600))
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, t0.Add(2*time.Second)))
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, t0.Add(3*time.Second)))
	node, err := ing.Store.NodeByPath(t.Context(), "/inbox/document.txt")
	require.NoError(t, err)
	assert.Equal(t, "document.txt", node.Name)
}

func TestWatcherRejectsVaultOverlapAndDestinationConflicts(t *testing.T) {
	ing := newTestIngester(t)
	ctx := t.Context()

	vaultRoot := t.TempDir()
	overlapping := filepath.Join(vaultRoot, "incoming")
	require.NoError(t, os.Mkdir(overlapping, 0o700))
	t.Run("vault", func(t *testing.T) {
		watcher, err := NewWatcher(ing, vaultRoot, config.WatchConfig{
			Name: "vault", Source: overlapping, Destination: "/inbox",
			SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Second),
		}, runTestMutation, nil)
		require.NoError(t, err)
		_, err = watcher.openRoot(ctx)
		require.ErrorContains(t, err, "overlaps the vault root")
	})

	source := writeTree(t, map[string]string{"conflict.txt": "watched"})
	parent, err := ing.Store.MkdirAll(t.Context(), "/inbox")
	require.NoError(t, err)
	hash, size, err := ing.Blobs.Write(stringsReader("existing"))
	require.NoError(t, err)
	_, err = ing.Store.CreateFile(t.Context(), parent.ID, "conflict.txt", hash, size, "text/plain")
	require.NoError(t, err)
	conflictPath := filepath.Join(source, "conflict.txt")
	fingerprint, err := observeLocalFileFingerprint(conflictPath)
	require.NoError(t, err)
	_, err = ing.ingestWatchedFile(
		ctx, "conflict", "/inbox", "conflict.txt", fingerprint,
		func() (*os.File, error) { return blob.OpenNoFollow(conflictPath) },
	)
	require.ErrorIs(t, err, store.ErrExists)
}

func TestPathsOverlapUsesFilesystemIdentityForAliases(t *testing.T) {
	vaultRoot := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(vaultRoot, "incoming"), 0o700))
	alias := filepath.Join(t.TempDir(), "vault-alias")
	if err := os.Symlink(vaultRoot, alias); err != nil {
		t.Skipf("creating a directory alias: %v", err)
	}
	aliasedChild := filepath.Join(alias, "incoming")
	assert.False(t, pathContains(vaultRoot, aliasedChild),
		"the test must exercise identity rather than lexical containment")

	overlaps, err := PathsOverlap(vaultRoot, aliasedChild)
	require.NoError(t, err)
	assert.True(t, overlaps)
}

func TestWatchBindingOverlapRejectsMissingPathThroughAlias(t *testing.T) {
	watchSource := t.TempDir()
	alias := filepath.Join(t.TempDir(), "watch-alias")
	if err := os.Symlink(watchSource, alias); err != nil {
		t.Skipf("creating a directory alias: %v", err)
	}
	bindingPath := filepath.Join(alias, "archive")
	assert.False(t, pathContains(watchSource, bindingPath),
		"the test must exercise identity rather than lexical containment")

	watchName, overlaps, err := WatchBindingOverlap(
		[]config.WatchConfig{{Name: "inbox", Source: watchSource}},
		config.StoreBindingConfig{Kind: "filesystem", Path: bindingPath},
	)
	require.NoError(t, err)
	assert.Equal(t, "inbox", watchName)
	assert.True(t, overlaps)
}

func TestWatchBindingOverlapRejectsMissingPathThroughDescendantAlias(t *testing.T) {
	watchSource := t.TempDir()
	watchedSubdir := filepath.Join(watchSource, "documents")
	require.NoError(t, os.Mkdir(watchedSubdir, 0o700))
	alias := filepath.Join(t.TempDir(), "documents-alias")
	if err := os.Symlink(watchedSubdir, alias); err != nil {
		t.Skipf("creating a directory alias: %v", err)
	}
	bindingPath := filepath.Join(alias, "archive")
	assert.False(t, pathContains(watchSource, bindingPath),
		"the test must exercise identity rather than lexical containment")

	watchName, overlaps, err := WatchBindingOverlap(
		[]config.WatchConfig{{Name: "inbox", Source: watchSource}},
		config.StoreBindingConfig{Kind: "filesystem", Path: bindingPath},
	)
	require.NoError(t, err)
	assert.Equal(t, "inbox", watchName)
	assert.True(t, overlaps)
}

func TestPathsOverlapUsesFilesystemIdentityForCaseAliases(t *testing.T) {
	parent := t.TempDir()
	vaultRoot := filepath.Join(parent, "CaseSensitiveWatch")
	require.NoError(t, os.Mkdir(vaultRoot, 0o700))
	alias := filepath.Join(parent, "casesensitivewatch")
	vaultInfo, err := os.Stat(vaultRoot)
	require.NoError(t, err)
	aliasInfo, err := os.Stat(alias)
	if os.IsNotExist(err) {
		t.Skip("filesystem is case-sensitive")
	}
	require.NoError(t, err)
	if !os.SameFile(vaultInfo, aliasInfo) {
		t.Skip("case variant does not identify the same directory")
	}

	overlaps, err := PathsOverlap(vaultRoot, alias)
	require.NoError(t, err)
	assert.True(t, overlaps)
}

func TestWatchTreeFindsVaultThroughUnrelatedAlias(t *testing.T) {
	backing := t.TempDir()
	vault := filepath.Join(backing, "private", "vault")
	require.NoError(t, os.MkdirAll(vault, 0o700))
	vaultAlias := filepath.Join(t.TempDir(), "vault-alias")
	if err := os.Symlink(vault, vaultAlias); err != nil {
		t.Skipf("creating a directory alias: %v", err)
	}
	assert.False(t, pathContains(backing, vaultAlias),
		"the alias must hide lexical ancestry")

	root, err := os.OpenRoot(backing)
	require.NoError(t, err)
	defer func() { require.NoError(t, root.Close()) }()
	mount, err := watchMountForRoot(root)
	require.NoError(t, err)
	vaultInfo, err := os.Stat(vaultAlias)
	require.NoError(t, err)

	contains, err := watchTreeContainsAnyDirectory(
		t.Context(), root, mount, []fs.FileInfo{vaultInfo}, exclusions{}, "", nil,
	)
	require.NoError(t, err)
	assert.True(t, contains)
}

func TestWatcherAliasCheckIgnoresEntryRemovedDuringStartup(t *testing.T) {
	source := writeTree(t, map[string]string{"vanishing/file.txt": "temporary"})
	watcher, err := NewWatcher(newTestIngester(t), t.TempDir(), config.WatchConfig{
		Name: "vanishing", Source: source, Destination: "/inbox",
		SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Second),
	}, runTestMutation, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	removed := false
	watcher.beforeDescend = func(rel string) {
		if removed || rel != "vanishing" {
			return
		}
		removed = true
		require.NoError(t, os.RemoveAll(filepath.Join(source, rel)))
	}
	root, err := watcher.openRoot(t.Context())
	require.NoError(t, err)
	require.NoError(t, root.Close())
	assert.True(t, removed)
}

func TestWatchDirectoryIdentitiesIncludeDescendants(t *testing.T) {
	vault := t.TempDir()
	logs := filepath.Join(vault, "logs")
	require.NoError(t, os.Mkdir(logs, 0o700))
	root, err := os.OpenRoot(vault)
	require.NoError(t, err)
	identities, err := watchDirectoryIdentities(t.Context(), root)
	require.NoError(t, err)
	require.NoError(t, root.Close())
	logsInfo, err := os.Stat(logs)
	require.NoError(t, err)
	assert.True(t, matchesWatchDirectory(logsInfo, identities))
}

func TestWatchedFileMustMatchItsSettledFingerprint(t *testing.T) {
	ing := newTestIngester(t)
	source := writeTree(t, map[string]string{"document.txt": "first"})
	sourcePath := filepath.Join(source, "document.txt")
	settled, err := observeLocalFileFingerprint(sourcePath)
	require.NoError(t, err)
	t.Run("change time only", func(t *testing.T) {
		source := writeTree(t, map[string]string{"document.txt": "first"})
		sourcePath := filepath.Join(source, "document.txt")
		local, err := observeLocalFileFingerprint(sourcePath)
		require.NoError(t, err)
		file, err := os.Open(sourcePath)
		require.NoError(t, err)
		local.change = fileChangeTime(file, local.info)
		require.NoError(t, file.Close())
		require.NotZero(t, local.change)
		rewriteSourceWithRestoredMtime(t, sourcePath, []byte("other"))
		_, err = ing.ingestWatchedFile(
			t.Context(), "documents", "/inbox", "document.txt", local,
			func() (*os.File, error) { return blob.OpenNoFollow(sourcePath) },
		)
		require.ErrorIs(t, err, ErrSourceChanged)
		_, err = ing.Store.NodeByPath(t.Context(), "/inbox/document.txt")
		require.ErrorIs(t, err, store.ErrNotFound)
	})
	replacementPath := filepath.Join(source, "replacement.tmp")
	require.NoError(t, os.WriteFile(replacementPath, []byte("other"), 0o600))
	observedAt := time.Unix(0, settled.modTime)
	require.NoError(t, os.Chtimes(replacementPath, observedAt, observedAt))
	require.NoError(t, os.Remove(sourcePath))
	require.NoError(t, os.Rename(replacementPath, sourcePath))
	replacement, err := observeLocalFileFingerprint(sourcePath)
	require.NoError(t, err)
	require.Equal(t, settled.size, replacement.size)
	require.Equal(t, settled.modTime, replacement.modTime)
	require.False(t, settled.matches(replacement))
	for _, tc := range []struct {
		left, right int64
		matches     bool
	}{{0, 1, false}, {1, 0, false}} {
		left, right := settled, settled
		left.change, right.change = tc.left, tc.right
		require.Equal(t, tc.matches, left.matches(right), "change times %d, %d", tc.left, tc.right)
	}

	_, err = ing.ingestWatchedFile(
		t.Context(), "documents", "/inbox", "document.txt", settled,
		func() (*os.File, error) { return blob.OpenNoFollow(sourcePath) },
	)
	require.ErrorIs(t, err, ErrSourceChanged)
	_, err = ing.Store.NodeByPath(t.Context(), "/inbox/document.txt")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestLocalFileReadRejectsConcurrentChange(t *testing.T) {
	ing := newTestIngester(t)
	filePath := filepath.Join(t.TempDir(), "changing.bin")
	require.NoError(t, os.WriteFile(filePath, make([]byte, progressByteInterval*2), 0o600))
	changed := false
	progress := newProgressTracker(func(event ProgressEvent) {
		if changed || event.BytesRead < progressByteInterval {
			return
		}
		changed = true
		require.NoError(t, os.WriteFile(filePath, []byte("changed"), 0o600))
	})
	_, err := ing.readLocalFile(context.Background(), filePath, filePath, progress, nil)
	require.ErrorIs(t, err, ErrSourceChanged)
	assert.True(t, changed)
}

func stringsReader(value string) io.Reader { return strings.NewReader(value) }

func runTestMutation(fn func() error) error { return fn() }

func TestWatchTreeTraversalHonorsContext(t *testing.T) {
	t.Parallel()
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("expired=%t", expired), func(t *testing.T) {
			root, err := os.OpenRoot(writeTree(t, map[string]string{"nested/file.txt": "content"}))
			require.NoError(t, err)
			defer func() { require.NoError(t, root.Close()) }()
			mount, err := watchMountForRoot(root)
			require.NoError(t, err)
			target, err := os.Stat(t.TempDir())
			require.NoError(t, err)
			var ctx context.Context
			var cancel context.CancelFunc
			want := context.Canceled
			if expired {
				ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
				want = context.DeadlineExceeded
			} else {
				ctx, cancel = context.WithCancel(t.Context())
				cancel()
			}
			defer cancel()
			_, err = watchTreeContainsAnyDirectory(ctx, root, mount, []fs.FileInfo{target}, exclusions{}, "", nil)
			require.ErrorIs(t, err, want)
		})
	}
}

func TestWatchedFileReadRejectsConcurrentPathReplacement(t *testing.T) {
	ing := newTestIngester(t)
	dir := t.TempDir()
	filePath := filepath.Join(dir, "document.bin")
	replacementPath := filepath.Join(dir, "replacement.tmp")
	content := make([]byte, progressByteInterval*2)
	require.NoError(t, os.WriteFile(filePath, content, 0o600))
	settled, err := observeLocalFileFingerprint(filePath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(replacementPath, content, 0o600))
	require.NoError(t, os.Chtimes(replacementPath,
		time.Unix(0, settled.modTime), time.Unix(0, settled.modTime)))

	replaced := false
	progress := newProgressTracker(func(event ProgressEvent) {
		if replaced || event.BytesRead < progressByteInterval {
			return
		}
		replaced = true
		require.NoError(t, os.Remove(filePath))
		require.NoError(t, os.Rename(replacementPath, filePath))
	})
	_, err = ing.readLocalFile(
		context.Background(), filePath, filePath, progress, &settled,
	)
	require.ErrorIs(t, err, ErrSourceChanged)
	assert.True(t, replaced)
}

func TestWatcherCheckoutAdmission(t *testing.T) {
	for _, tc := range []struct {
		name    string
		retired bool
		files   bool
	}{
		{name: "reserved blocks until cancel"},
		{name: "retired empty admits immediately", retired: true},
		{name: "retired files wait until cleared", retired: true, files: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ing := newTestIngester(t)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				destination := t.TempDir()
				watchSource := destination
				if tc.files {
					destination = filepath.Join(watchSource, "checkout")
					require.NoError(t, os.Mkdir(destination, 0o700))
				}
				node, err := ing.Store.CreateFile(ctx, ing.Store.RootID(), "photo.jpg", strings.Repeat("a", 64), 20, "image/jpeg")
				require.NoError(t, err)
				asset, err := ing.Store.PhotoAssetForNode(ctx, node.ID)
				require.NoError(t, err)
				checkout, op, _, err := ing.Store.CreatePhotoCheckout(ctx, store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, destination, "", 0)
				require.NoError(t, err)
				if tc.retired {
					require.NoError(t, ing.Store.FinishPhotoCheckout(ctx, checkout.ID, op.ID, store.StorageOperationCompleted, "{}", "", time.Now().Add(time.Hour)))
					_, err = ing.Store.RetirePhotoCheckout(ctx, checkout.ID)
					require.NoError(t, err)
				}
				partial := filepath.Join(destination, "edited.jpg")
				if tc.files {
					require.NoError(t, os.WriteFile(partial, []byte("edits"), 0o600))
				}
				var log bytes.Buffer
				watcher, err := NewWatcher(ing, t.TempDir(), config.WatchConfig{Name: "inbox", Source: watchSource, Destination: "/inbox", ScanInterval: config.Duration(time.Hour)}, runTestMutation, slog.New(slog.NewTextHandler(&log, nil)))
				require.NoError(t, err)
				done := make(chan error, 1)
				var supervisor *jobs.Supervisor
				if tc.files {
					supervisor = jobs.New(ctx, nil)
					defer func() { require.NoError(t, supervisor.Shutdown(context.Background())) }()
					require.NoError(t, supervisor.Start("watch:inbox", watcher.Run))
				} else {
					go func() {
						root, err := watcher.openRoot(ctx)
						if err == nil {
							err = root.Close()
						}
						done <- err
					}()
				}
				synctest.Wait()
				if !tc.retired || tc.files {
					if !tc.retired {
						select {
						case err := <-done:
							t.Fatalf("watch stopped before checkout cleared: %v", err)
						default:
						}
						cancel()
						require.ErrorIs(t, <-done, context.Canceled)
						return
					}
					snapshot := supervisor.Snapshot()[0]
					require.Equal(t, jobs.StatusRunning, snapshot.Status)
					require.Contains(t, snapshot.Error, destination)
					require.Contains(t, snapshot.Error, checkout.ID)
					require.Contains(t, snapshot.Error, "clear or move")
					time.Sleep(2 * time.Minute)
					synctest.Wait()
					require.Equal(t, 1, strings.Count(log.String(), "watch is waiting"))
					require.NoError(t, os.Remove(partial))
				} else {
					select {
					case err := <-done:
						require.NoError(t, err)
					default:
						t.Fatal("clear watch source did not admit immediately")
					}
				}
				if tc.files {
					time.Sleep(30 * time.Second)
					synctest.Wait()
					snapshot := supervisor.Snapshot()[0]
					require.Equal(t, jobs.StatusRunning, snapshot.Status)
					require.Empty(t, snapshot.Error)
					released, err := ing.Store.PhotoCheckout(ctx, checkout.ID)
					require.NoError(t, err)
					require.Empty(t, released.Destination)
				}
			})
		})
	}
}

func TestWatchExcludePatternsRejectInvalidConfiguration(t *testing.T) {
	for _, pattern := range []string{"[", `a\*.swp`} {
		cfg := config.WatchConfig{Name: "invalid-pattern", Source: t.TempDir(), Destination: "/archive", SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Second), ExcludePatterns: []string{pattern}}
		_, err := NewWatcher(newTestIngester(t), t.TempDir(), cfg, runTestMutation, slog.New(slog.DiscardHandler))
		require.ErrorContains(t, err, `watch "invalid-pattern" exclusions`)
		_, err = NewFolderScanner(cfg, func(context.Context, FolderFile) error { return nil }, failOnReject(t))
		require.ErrorContains(t, err, `watch "invalid-pattern" exclusions`)
	}
}

func TestWatcherExcludePatternsSkipFilesAndPruneDirectories(t *testing.T) {
	ing := newTestIngester(t)
	source := writeTree(t, map[string]string{"note.md": "note", ".syncthing.note.md.tmp": "partial", "a/b/x.swp": "swap", "cache-1/data.bin": "cached", "keep/real.md": "real"})
	watcher, err := NewWatcher(ing, t.TempDir(), config.WatchConfig{Name: "patterns", Source: source, Destination: "/archive", SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Second), ExcludePatterns: []string{".syncthing.*.tmp", "*.swp", "cache-*"}}, runTestMutation, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	var excluded, descended []string
	watcher.excluded = func(_ context.Context, ref string) error {
		excluded = append(excluded, filepath.ToSlash(ref))
		return nil
	}
	watcher.beforeDescend = func(ref string) { descended = append(descended, filepath.ToSlash(ref)) }
	root := openWatcherRoot(t, watcher)
	at := time.Unix(1_700_000_000, 0)
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, at))
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, at.Add(time.Second)))
	for _, ref := range []string{"note.md", "keep/real.md"} {
		_, err := ing.Store.NodeByPath(t.Context(), "/archive/"+ref)
		require.NoError(t, err)
	}
	for _, ref := range []string{".syncthing.note.md.tmp", "a/b/x.swp", "cache-1"} {
		assert.Contains(t, excluded, ref)
		_, err := ing.Store.NodeByPath(t.Context(), "/archive/"+ref)
		require.Error(t, err)
	}
	assert.NotContains(t, descended, "cache-1")
	assert.NotContains(t, excluded, "cache-1/data.bin")
}

func TestWatchTreeExcludePatternsPruneStartupTraversal(t *testing.T) {
	source := writeTree(t, map[string]string{"cache-1/inner/data.txt": "cached"})
	root, err := os.OpenRoot(source)
	require.NoError(t, err)
	defer func() { require.NoError(t, root.Close()) }()
	mount, err := watchMountForRoot(root)
	require.NoError(t, err)
	target, err := os.Stat(filepath.Join(source, "cache-1", "inner"))
	require.NoError(t, err)
	excludes, err := compileExclusions(nil, []string{"cache-*"})
	require.NoError(t, err)
	var descended []string
	contains, err := watchTreeContainsAnyDirectory(t.Context(), root, mount, []fs.FileInfo{target}, excludes, "", func(rel string) { descended = append(descended, rel) })
	require.NoError(t, err)
	assert.False(t, contains)
	assert.NotContains(t, descended, "cache-1")
}

func TestWatchCaptureOnChangeAdmission(t *testing.T) {
	for _, tc := range []struct {
		name       string
		prompt     bool
		minimumAge time.Duration
		retry      bool
	}{
		{name: "prompt", prompt: true},
		{name: "settled"},
		{name: "prompt minimum age", prompt: true, minimumAge: time.Hour},
		{name: "prompt transient retry", prompt: true, retry: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ing := newTestIngester(t)
			source := writeTree(t, map[string]string{"notes.txt": "first"})
			pathname := filepath.Join(source, "notes.txt")
			attempts := 0
			watcher, err := NewWatcher(ing, t.TempDir(), config.WatchConfig{
				Name: "notes", Source: source, Destination: "/archive", CaptureOnChange: tc.prompt,
				SettleTime: config.Duration(10 * time.Second), MinimumAge: config.Duration(tc.minimumAge), ScanInterval: config.Duration(time.Hour),
			}, func(fn func() error) error {
				attempts++
				if tc.retry && attempts == 1 {
					return ErrRetryLater
				}
				return fn()
			}, slog.New(slog.DiscardHandler))
			require.NoError(t, err)
			root := openWatcherRoot(t, watcher)
			now := time.Now().Add(2 * time.Hour)
			if tc.minimumAge > 0 {
				now = time.Now()
			}
			require.NoError(t, scanWatcherAt(t.Context(), watcher, root, now))
			_, err = ing.Store.NodeByPath(t.Context(), "/archive/notes.txt")
			if !tc.prompt || tc.minimumAge > 0 || tc.retry {
				require.ErrorIs(t, err, store.ErrNotFound)
			} else {
				require.NoError(t, err)
			}
			if tc.retry {
				now = now.Add(time.Second)
				require.NoError(t, scanWatcherAt(t.Context(), watcher, root, now))
			}
			if tc.minimumAge > 0 {
				now = time.Now().Add(3 * time.Hour)
				require.NoError(t, scanWatcherAt(t.Context(), watcher, root, now))
			} else if !tc.prompt {
				now = now.Add(10 * time.Second)
				require.NoError(t, scanWatcherAt(t.Context(), watcher, root, now))
			}
			first, err := ing.Store.NodeByPath(t.Context(), "/archive/notes.txt")
			require.NoError(t, err)
			rewriteSourceWithRestoredMtime(t, pathname, []byte("other"))
			if tc.minimumAge > 0 {
				require.NoError(t, os.Chtimes(pathname, now, now))
			}
			now = now.Add(time.Second)
			require.NoError(t, scanWatcherAt(t.Context(), watcher, root, now))
			_, count, err := ing.Store.ContentVersions(t.Context(), first.ID, 10, 0)
			require.NoError(t, err)
			if tc.prompt && tc.minimumAge == 0 {
				require.Equal(t, 2, count)
			} else {
				require.Equal(t, 1, count)
			}
			if count == 1 {
				now = now.Add(10*time.Second + tc.minimumAge)
				require.NoError(t, scanWatcherAt(t.Context(), watcher, root, now))
			}
			second, err := ing.Store.NodeByPath(t.Context(), "/archive/notes.txt")
			require.NoError(t, err)
			require.Equal(t, first.ID, second.ID)
			require.NotEqual(t, first.CurrentVersionID, second.CurrentVersionID)
			require.NoError(t, scanWatcherAt(t.Context(), watcher, root, now))
			_, count, err = ing.Store.ContentVersions(t.Context(), first.ID, 10, 0)
			require.NoError(t, err)
			require.Equal(t, 2, count)
		})
	}
}

func TestPromptWatchDirtyHintsStayConfined(t *testing.T) {
	ing := newTestIngester(t)
	source := writeTree(t, map[string]string{"safe.txt": "safe", "blocked.txt": "blocked"})
	outside := writeTree(t, map[string]string{"secret.txt": "outside"})
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(source, "link.txt")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	watcher, err := NewWatcher(ing, t.TempDir(), config.WatchConfig{
		Name: "notes", Source: source, Destination: "/archive", CaptureOnChange: true,
		SettleTime: config.Duration(time.Hour), ScanInterval: config.Duration(time.Hour),
		Exclude: []string{"blocked.txt"},
	}, runTestMutation, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	root := openWatcherRoot(t, watcher)
	requireScanComplete(t, checkedScan(watcher.scanWithHints(t.Context(), root, watchScanHints{Reconcile: true})))
	requireScanComplete(t, checkedScan(watcher.scanWithHints(t.Context(), root, watchScanHints{
		Dirty: map[string]struct{}{"blocked.txt": {}, "link.txt": {}},
	})))
	for _, ref := range []string{"blocked.txt", "link.txt", "secret.txt"} {
		_, err := ing.Store.NodeByPath(t.Context(), "/archive/"+ref)
		require.ErrorIs(t, err, store.ErrNotFound)
	}
	renameWatcherRoot(t, root, source+"-moved")
	require.NoError(t, os.Symlink(outside, source))
	requireScanErrorIs(t, checkedScan(watcher.scanWithHints(t.Context(), root, watchScanHints{
		Dirty: map[string]struct{}{"safe.txt": {}},
	})), ErrSourceChanged)
	_, err = ing.Store.NodeByPath(t.Context(), "/archive/secret.txt")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestPromptWatchDirtyUnchangedSourcePreservesManualHeadAcrossRestart(t *testing.T) {
	ing := newTestIngester(t)
	source := writeTree(t, map[string]string{"notes.txt": "source"})
	cfg := config.WatchConfig{Name: "notes", Source: source, Destination: "/archive",
		CaptureOnChange: true, SettleTime: config.Duration(time.Hour), ScanInterval: config.Duration(time.Hour)}
	watcher, err := NewWatcher(ing, t.TempDir(), cfg, runTestMutation, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	root := openWatcherRoot(t, watcher)
	require.NoError(t, watcher.scan(t.Context(), root))
	node, err := ing.Store.NodeByPath(t.Context(), "/archive/notes.txt")
	require.NoError(t, err)
	hash, size, err := ing.Blobs.Write(strings.NewReader("manual"))
	require.NoError(t, err)
	manual, _, err := ing.Store.ReplaceContent(t.Context(), node.ID, node.Revision, hash, size, "text/plain")
	require.NoError(t, err)
	requireScanComplete(t, checkedScan(watcher.scanWithHints(t.Context(), root, watchScanHints{
		Dirty: map[string]struct{}{"notes.txt": {}},
	})))
	after, err := ing.Store.NodeByID(t.Context(), node.ID)
	require.NoError(t, err)
	require.Equal(t, manual, after)
	restarted, err := NewWatcher(ing, t.TempDir(), cfg, runTestMutation, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	nextRoot := openWatcherRoot(t, restarted)
	require.NoError(t, restarted.scan(t.Context(), nextRoot))
	after, err = ing.Store.NodeByID(t.Context(), node.ID)
	require.NoError(t, err)
	require.Equal(t, manual, after)
}

func TestWatcherLiveRenameKeepsNodeAndHistory(t *testing.T) {
	ing := newTestIngester(t)
	source := writeTree(t, map[string]string{"draft.md": "first"})
	requireRenameEvidence(t, filepath.Join(source, "draft.md"))
	watcher, err := NewWatcher(ing, t.TempDir(), config.WatchConfig{Name: "notes", Source: source, Destination: "/archive", SettleTime: config.Duration(10 * time.Second), ScanInterval: config.Duration(time.Second)}, runTestMutation, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	root := openWatcherRoot(t, watcher)
	at := time.Unix(1700000000, 0)
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, at))
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, at.Add(10*time.Second)))
	original, err := ing.Store.NodeByPath(t.Context(), "/archive/draft.md")
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(filepath.Join(source, "notes"), 0o755))
	require.NoError(t, os.Rename(filepath.Join(source, "draft.md"), filepath.Join(source, "notes", "final.md")))
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, at.Add(11*time.Second)))
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, at.Add(21*time.Second)))
	cursors, err := ing.Store.PushCursors(t.Context(), "notes", "", 10)
	require.NoError(t, err)
	require.Len(t, cursors, 1)
	assert.Equal(t, "notes/final.md", cursors[0].Ref)
	assert.Equal(t, original.ID, cursors[0].NodeID)
	_, total, err := ing.Store.ContentVersions(t.Context(), original.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.NoError(t, os.WriteFile(filepath.Join(source, "notes", "final.md"), []byte("second"), 0o644))
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, at.Add(22*time.Second)))
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, at.Add(32*time.Second)))
	_, total, err = ing.Store.ContentVersions(t.Context(), original.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, total)
	versions, _, err := ing.Store.ContentVersions(t.Context(), original.ID, 10, 0)
	require.NoError(t, err)
	bytesByVersion := make(map[string]string)
	for _, version := range versions {
		reader, openErr := ing.Blobs.Open(version.BlobHash)
		require.NoError(t, openErr)
		data, readErr := io.ReadAll(reader)
		require.NoError(t, readErr)
		require.NoError(t, reader.Close())
		bytesByVersion[version.ID] = string(data)
	}
	assert.Equal(t, "first", bytesByVersion[original.CurrentVersionID])
	cfg := watcher.config
	restarted, err := NewWatcher(ing, t.TempDir(), cfg, runTestMutation, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	nextRoot := openWatcherRoot(t, restarted)
	require.NoError(t, scanWatcherAt(t.Context(), restarted, nextRoot, at.Add(40*time.Second)))
	require.NoError(t, scanWatcherAt(t.Context(), restarted, nextRoot, at.Add(50*time.Second)))
	cursors, err = ing.Store.PushCursors(t.Context(), "notes", "", 10)
	require.NoError(t, err)
	require.Len(t, cursors, 1)
	assert.Equal(t, original.ID, cursors[0].NodeID)
	_, total, err = ing.Store.ContentVersions(t.Context(), original.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, total)
}

func TestWatcherRenameAndEditBeforeFirstScanKeepsNode(t *testing.T) {
	ing := newTestIngester(t)
	source := writeTree(t, map[string]string{"draft.md": "first"})
	requireRenameEvidence(t, filepath.Join(source, "draft.md"))
	watcher, err := NewWatcher(ing, t.TempDir(), config.WatchConfig{
		Name: "notes", Source: source, Destination: "/archive",
		SettleTime: config.Duration(10 * time.Second), ScanInterval: config.Duration(time.Second),
	}, runTestMutation, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	root := openWatcherRoot(t, watcher)
	at := time.Unix(1700000000, 0)
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, at))
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, at.Add(10*time.Second)))
	original, err := ing.Store.NodeByPath(t.Context(), "/archive/draft.md")
	require.NoError(t, err)
	require.NoError(t, os.Rename(filepath.Join(source, "draft.md"), filepath.Join(source, "final.md")))
	require.NoError(t, os.WriteFile(filepath.Join(source, "final.md"), []byte("second longer"), 0o644))
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, at.Add(11*time.Second)))
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, at.Add(21*time.Second)))
	cursors, err := ing.Store.PushCursors(t.Context(), "notes", "", 10)
	require.NoError(t, err)
	require.Len(t, cursors, 1)
	assert.Equal(t, original.ID, cursors[0].NodeID)
	assert.Equal(t, "final.md", cursors[0].Ref)
	_, total, err := ing.Store.ContentVersions(t.Context(), original.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, total)
}
