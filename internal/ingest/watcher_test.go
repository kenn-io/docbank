package ingest

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

func openWatcherRoot(t *testing.T, watcher *Watcher) *watchRoot {
	t.Helper()
	root, err := watcher.openRoot(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })
	return root
}

func scanWatcherAt(ctx context.Context, watcher *Watcher, root *watchRoot, at time.Time) error {
	previous := watcher.now
	watcher.now = func() time.Time { return at }
	defer func() { watcher.now = previous }()
	return watcher.scan(ctx, root)
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
	vaultRoot := t.TempDir()
	overlapping := filepath.Join(vaultRoot, "incoming")
	require.NoError(t, os.Mkdir(overlapping, 0o700))
	watcher, err := NewWatcher(ing, vaultRoot, config.WatchConfig{
		Name: "overlap", Source: overlapping, Destination: "/inbox",
		SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Second),
	}, runTestMutation, nil)
	require.NoError(t, err)
	_, err = watcher.openRoot(t.Context())
	require.ErrorContains(t, err, "overlaps the vault root")

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
		t.Context(), "conflict", "/inbox", "conflict.txt", fingerprint,
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

	overlaps, err := pathsOverlap(vaultRoot, aliasedChild)
	require.NoError(t, err)
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

	overlaps, err := pathsOverlap(vaultRoot, alias)
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

func stringsReader(value string) io.Reader { return strings.NewReader(value) }

func runTestMutation(fn func() error) error { return fn() }
