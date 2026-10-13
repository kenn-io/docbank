package checkout

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
)

func checkoutService(t *testing.T) Service {
	t.Helper()
	vault := t.TempDir()
	catalog, err := store.Open(filepath.Join(vault, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(vault, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	return Service{Store: catalog, Blobs: blobs, VaultRoot: vault, Config: config.Default()}
}

func checkoutPhoto(t *testing.T, s Service, name, content string) store.PhotoAsset {
	t.Helper()
	hash, size, err := s.Blobs.Write(strings.NewReader(content))
	require.NoError(t, err)
	node, err := s.Store.CreateFile(t.Context(), s.Store.RootID(), name, hash, size, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.Store.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	return asset
}

func TestCheckoutCopiesVerifiedWritableBases(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	a := checkoutPhoto(t, s, "photo.jpg", "original bytes")
	destination := t.TempDir()
	c, op, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, destination, 0)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	// The run must copy the frozen base even when the source head changes.
	node, err := s.Store.NodeByID(t.Context(), a.Files[0].NodeID)
	require.NoError(t, err)
	hash, size, err := s.Blobs.Write(strings.NewReader("new bytes"))
	require.NoError(t, err)
	_, _, err = s.Store.ReplaceContent(t.Context(), node.ID, node.Revision, hash, size, "image/jpeg")
	require.NoError(t, err)
	require.NoError(t, s.Run(t.Context(), c, root))
	entries, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.True(t, entries[0].Published)
	content, err := root.ReadFile(filepath.FromSlash(entries[0].Path))
	require.NoError(t, err)
	require.Equal(t, "original bytes", string(content))
	require.NoError(t, root.WriteFile(filepath.FromSlash(entries[0].Path), []byte("edited"), 0o600))
	finished, err := s.Store.StorageOperation(t.Context(), op.ID)
	require.NoError(t, err)
	require.Equal(t, store.StorageOperationCompleted, finished.State)
	require.Equal(t, int64(1), finished.CopiedObjects)
	require.Equal(t, int64(len(content)), finished.CopiedBytes)
	status, err := s.Store.PhotoCheckout(t.Context(), c.ID)
	require.NoError(t, err)
	require.Equal(t, "completed", status.State)
}

func TestCheckoutPartialFailureKeepsCopiesAndCollision(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	checkoutPhoto(t, s, "one.jpg", "one")
	checkoutPhoto(t, s, "two.jpg", "two")
	c, op, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{All: true}, t.TempDir(), 100)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	entries, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	collision := filepath.FromSlash(entries[1].Path)
	require.NoError(t, root.MkdirAll(filepath.Dir(collision), 0o755))
	require.NoError(t, root.WriteFile(collision, []byte("keep my edit"), 0o600))
	require.Error(t, s.Run(t.Context(), c, root))
	entries, err = s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
	require.NoError(t, err)
	require.True(t, entries[0].Published)
	require.False(t, entries[1].Published)
	content, err := root.ReadFile(collision)
	require.NoError(t, err)
	require.Equal(t, "keep my edit", string(content))
	finished, err := s.Store.StorageOperation(t.Context(), op.ID)
	require.NoError(t, err)
	require.Equal(t, store.StorageOperationFailed, finished.State)
	require.Equal(t, int64(1), finished.CompletedObjects)
}

func TestCheckoutInterruptedRunKeepsIdentityAndPartialResults(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	checkoutPhoto(t, s, "one.jpg", "one")
	checkoutPhoto(t, s, "two.jpg", "two")
	c, op, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{All: true}, t.TempDir(), 100)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.Mutate = func(_ context.Context, fn func() error) error { err := fn(); cancel(); return err }
	require.ErrorIs(t, s.Run(ctx, c, root), context.Canceled)
	require.NoError(t, s.reconcile(t.Context(), c))
	finished, err := s.Store.StorageOperation(t.Context(), op.ID)
	require.NoError(t, err)
	require.Equal(t, op.ID, finished.ID)
	require.Equal(t, store.StorageOperationFailed, finished.State)
	require.Contains(t, finished.Error, "interrupted")
	entries, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
	require.NoError(t, err)
	require.True(t, entries[0].Published)
	require.False(t, entries[1].Published)
	_, err = root.ReadFile(filepath.FromSlash(entries[0].Path))
	require.NoError(t, err)
	status, err := s.Store.PhotoCheckout(t.Context(), c.ID)
	require.NoError(t, err)
	require.Equal(t, "failed", status.State)
}

func TestCheckoutCancellation(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	a := checkoutPhoto(t, s, "photo.jpg", "bytes")
	c, op, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, t.TempDir(), 0)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	require.NoError(t, s.Store.RequestStorageOperationCancel(t.Context(), op.ID))
	require.NoError(t, s.Run(t.Context(), c, root))
	finished, err := s.Store.StorageOperation(t.Context(), op.ID)
	require.NoError(t, err)
	require.Equal(t, store.StorageOperationCancelled, finished.State)
	entries, err := os.ReadDir(c.Destination)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestCheckoutDestinationSafety(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	a := checkoutPhoto(t, s, "photo.jpg", "bytes")
	selection := store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}
	storageRoot, watchRoot := t.TempDir(), t.TempDir()
	s.Config.StoreBindings = map[string]config.StoreBindingConfig{"archive": {Kind: "filesystem", Path: storageRoot}}
	s.Config.Watches = []config.WatchConfig{{Source: watchRoot}}
	for _, base := range []string{s.VaultRoot, storageRoot, watchRoot} {
		destination := filepath.Join(base, "checkout")
		require.NoError(t, os.Mkdir(destination, 0o755))
		_, _, _, err := s.Prepare(t.Context(), selection, destination, 0)
		require.ErrorIs(t, err, store.ErrInvalidPhotoCheckout)
	}
	nonempty := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(nonempty, "keep"), nil, 0o600))
	_, _, _, err := s.Prepare(t.Context(), selection, nonempty, 0)
	require.ErrorIs(t, err, store.ErrInvalidPhotoCheckout)
	_, _, _, err = s.Prepare(t.Context(), selection, "relative", 0)
	require.ErrorIs(t, err, store.ErrInvalidPhotoCheckout)
	_, _, _, err = s.Prepare(t.Context(), selection, filepath.Join(t.TempDir(), "missing"), 0)
	require.Error(t, err)
	c, _, root, err := s.Prepare(t.Context(), selection, t.TempDir(), 0)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	sub := filepath.Join(c.Destination, "nested")
	require.NoError(t, os.Mkdir(sub, 0o755))
	_, _, _, err = s.Prepare(t.Context(), selection, sub, 0)
	require.ErrorIs(t, err, store.ErrInvalidPhotoCheckout)
}

func TestCheckoutRefusesCorruptBytes(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	a := checkoutPhoto(t, s, "photo.jpg", "bytes")
	c, _, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, t.TempDir(), 0)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	node, err := s.Store.NodeByID(t.Context(), a.Files[0].NodeID)
	require.NoError(t, err)
	loose := filepath.Join(s.VaultRoot, "blobs", node.BlobHash[:2], node.BlobHash)
	require.NoError(t, os.WriteFile(loose, []byte("wrong"), 0o600))
	err = s.Run(t.Context(), c, root)
	require.Error(t, err)
	require.False(t, errors.Is(err, context.Canceled))
	entries, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
	require.NoError(t, err)
	require.False(t, entries[0].Published)
}

func TestCheckoutCollidingOriginalNamesFailDuringCopy(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	first := checkoutPhoto(t, s, "photo.jpg", "first")
	dir, err := s.Store.Mkdir(t.Context(), s.Store.RootID(), "other")
	require.NoError(t, err)
	hash, size, err := s.Blobs.Write(strings.NewReader("first"))
	require.NoError(t, err)
	second, err := s.Store.CreateFile(t.Context(), dir.ID, "photo.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	owned, err := s.Store.PhotoAssetForNode(t.Context(), second.ID)
	require.NoError(t, err)
	_, err = s.Store.DetachPhotoFile(t.Context(), owned.ID, owned.Revision, owned.Files[0].ID, store.PhotoDetachOptions{})
	require.NoError(t, err)
	_, err = s.Store.AttachPhotoFile(t.Context(), first.ID, first.Revision, second.ID, store.PhotoRoleImage, nil)
	require.NoError(t, err)
	c, op, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{first.ID}}, t.TempDir(), 0)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	entries, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.Equal(t, entries[0].Path, entries[1].Path)
	require.Error(t, s.Run(t.Context(), c, root))
	actual, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
	require.NoError(t, err)
	require.True(t, actual[0].Published)
	require.False(t, actual[1].Published)
	finished, err := s.Store.StorageOperation(t.Context(), op.ID)
	require.NoError(t, err)
	require.Equal(t, store.StorageOperationFailed, finished.State)
}

func TestCheckoutAllowsUnrelatedMissingRoots(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	a := checkoutPhoto(t, s, "photo.jpg", "bytes")
	selection := store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}
	missing := filepath.Join(t.TempDir(), "missing")
	s.Config.StoreBindings = map[string]config.StoreBindingConfig{"archive": {Kind: "filesystem", Path: filepath.Join(missing, "archive")}}
	s.Config.Watches = []config.WatchConfig{{Source: filepath.Join(missing, "watch")}}
	_, _, err := s.Store.CreatePhotoCheckout(t.Context(), selection, filepath.Join(missing, "checkout"), 0)
	require.NoError(t, err)
	_, _, root, err := s.Prepare(t.Context(), selection, t.TempDir(), 0)
	require.NoError(t, err)
	require.NoError(t, root.Close())
	destination := t.TempDir()
	s.Config.Watches = []config.WatchConfig{{Source: filepath.Join(destination, "missing-descendant")}}
	_, _, _, err = s.Prepare(t.Context(), selection, destination, 0)
	require.ErrorIs(t, err, store.ErrInvalidPhotoCheckout)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(destination, alias); err != nil {
		t.Skip("symlink creation unavailable")
	}
	s.Config.Watches = []config.WatchConfig{{Source: filepath.Join(alias, "missing-descendant")}}
	_, _, _, err = s.Prepare(t.Context(), selection, destination, 0)
	require.ErrorIs(t, err, store.ErrInvalidPhotoCheckout)
}

func TestCheckoutRecoveryRemovesOnlyExactEmptyableStages(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	a := checkoutPhoto(t, s, "photo.jpg", "bytes")
	c, _, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, t.TempDir(), 0)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	entries, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
	require.NoError(t, err)
	prefix := ".docbank-checkout-"
	payload := prefix + strings.Repeat("a", 32)
	decoy := prefix + strings.Repeat("c", 32)
	wrongName := prefix + "user-file"
	require.NoError(t, root.WriteFile(payload, []byte("partial bytes"), 0600))
	require.NoError(t, root.Mkdir(decoy, 0700))
	require.NoError(t, root.WriteFile(filepath.Join(decoy, "my-edit.jpg"), []byte("keep"), 0600))
	require.NoError(t, root.WriteFile(wrongName, []byte("keep"), 0600))
	require.NoError(t, root.MkdirAll(filepath.Dir(filepath.FromSlash(entries[0].Path)), 0755))
	require.NoError(t, root.WriteFile(filepath.FromSlash(entries[0].Path), []byte("published copy"), 0600))
	require.NoError(t, s.reconcile(t.Context(), c))
	_, err = root.Stat(payload)
	require.ErrorIs(t, err, os.ErrNotExist)
	for _, name := range []string{decoy, wrongName} {
		_, err = root.Stat(name)
		require.NoError(t, err)
	}
	content, err := root.ReadFile(filepath.FromSlash(entries[0].Path))
	require.NoError(t, err)
	require.Equal(t, "published copy", string(content))
	status, err := s.Store.PhotoCheckout(t.Context(), c.ID)
	require.NoError(t, err)
	require.Equal(t, "failed", status.State)
}

func TestCheckoutStartFailurePersistsFailureAndClosesRoot(t *testing.T) {
	t.Parallel()
	for _, stopped := range []bool{false, true} {
		t.Run(fmt.Sprint(stopped), func(t *testing.T) {
			s := checkoutService(t)
			a := checkoutPhoto(t, s, "photo.jpg", "bytes")
			c, op, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, t.TempDir(), 0)
			require.NoError(t, err)
			var supervisor *jobs.Supervisor
			if stopped {
				supervisor = jobs.New(t.Context(), nil)
				supervisor.Stop()
			}
			require.Error(t, s.Start(supervisor, c, root))
			_, err = root.Stat(".")
			require.Error(t, err)
			status, err := s.Store.PhotoCheckout(t.Context(), c.ID)
			require.NoError(t, err)
			require.Equal(t, "failed", status.State)
			finished, err := s.Store.StorageOperation(t.Context(), op.ID)
			require.NoError(t, err)
			require.Equal(t, store.StorageOperationFailed, finished.State)
			require.Equal(t, finished.Error, status.Error)
			require.NotNil(t, finished.FinishedAt)
		})
	}
}

func TestCheckoutDestinationMoveFailsAndRetainsPartialCopies(t *testing.T) {
	t.Parallel()
	for _, when := range []string{"after first copy", "before completion"} {
		t.Run(when, func(t *testing.T) {
			s := checkoutService(t)
			checkoutPhoto(t, s, "one.jpg", "one")
			if when == "after first copy" {
				checkoutPhoto(t, s, "two.jpg", "two")
			}
			c, op, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{All: true}, t.TempDir(), 100)
			require.NoError(t, err)
			defer func() { _ = root.Close() }()
			moved := filepath.Join(t.TempDir(), "moved")
			calls := 0
			s.Mutate = func(_ context.Context, fn func() error) error {
				calls++
				if calls > 1 {
					return fn()
				}
				move := func() {
					err := os.Rename(c.Destination, moved)
					if err != nil && runtime.GOOS == "windows" {
						t.Skip("Windows pins the opened destination against rename")
					}
					require.NoError(t, err)
					require.NoError(t, os.Mkdir(c.Destination, 0o755))
				}
				err := fn()
				move()
				return err
			}
			require.ErrorIs(t, s.Run(t.Context(), c, root), store.ErrInvalidPhotoCheckout)
			status, err := s.Store.PhotoCheckout(t.Context(), c.ID)
			require.NoError(t, err)
			require.Equal(t, "failed", status.State)
			finished, err := s.Store.StorageOperation(t.Context(), op.ID)
			require.NoError(t, err)
			require.Equal(t, store.StorageOperationFailed, finished.State)
			entries, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
			require.NoError(t, err)
			require.True(t, entries[0].Published)
			content, err := root.ReadFile(filepath.FromSlash(entries[0].Path))
			require.NoError(t, err)
			require.NotEmpty(t, content)
			names, err := os.ReadDir(c.Destination)
			require.NoError(t, err)
			require.Empty(t, names)
		})
	}
}

func TestCheckoutRecoveryContinuesAfterDestinationBecomesFile(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	a := checkoutPhoto(t, s, "photo.jpg", "bytes")
	selection := store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}
	first, _, root, err := s.Prepare(t.Context(), selection, t.TempDir(), 0)
	require.NoError(t, err)
	require.NoError(t, root.Close())
	require.NoError(t, os.Remove(first.Destination))
	require.NoError(t, os.WriteFile(first.Destination, []byte("keep this file"), 0o600))
	second, _, root, err := s.Prepare(t.Context(), selection, t.TempDir(), 0)
	// The inaccessible prior reservation is still checked for overlap.
	require.NoError(t, err)
	require.NoError(t, root.Close())
	for _, c := range []store.PhotoCheckout{first, second} {
		require.NoError(t, s.reconcile(t.Context(), c))
		status, err := s.Store.PhotoCheckout(t.Context(), c.ID)
		require.NoError(t, err)
		require.Equal(t, "failed", status.State)
		require.Contains(t, status.Error, "interrupted")
	}
	content, err := os.ReadFile(first.Destination)
	require.NoError(t, err)
	require.Equal(t, "keep this file", string(content))
}

func TestCheckoutRecoveryRebuildsCopyObservationsAfterPublicationCrash(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	for _, name := range []string{"one.jpg", "two.jpg", "three.jpg"} {
		checkoutPhoto(t, s, name, name)
	}
	c, op, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{All: true}, t.TempDir(), 100)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	entries, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
	require.NoError(t, err)
	require.Len(t, entries, 3)
	for _, e := range entries {
		require.NoError(t, root.MkdirAll(filepath.Dir(filepath.FromSlash(e.Path)), 0o755))
	}
	stream, _, err := s.Blobs.OpenStreamContext(t.Context(), entries[0].Hash)
	require.NoError(t, err)
	bytes, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	// A crash after rename can leave verified bytes without a committed observation.
	require.NoError(t, root.WriteFile(filepath.FromSlash(entries[0].Path), bytes, 0o600))
	edited := []byte(strings.Repeat("x", int(entries[1].Size)))
	require.NoError(t, root.WriteFile(filepath.FromSlash(entries[1].Path), edited, 0o600))
	require.NoError(t, s.Store.PublishPhotoCheckoutEntry(t.Context(), entries[2], func() (bool, error) { return true, nil }))
	require.NoError(t, s.reconcile(t.Context(), c))
	observed, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
	require.NoError(t, err)
	require.True(t, observed[0].Published)
	require.False(t, observed[1].Published)
	require.False(t, observed[2].Published)
	actual, err := root.ReadFile(filepath.FromSlash(entries[1].Path))
	require.NoError(t, err)
	require.Equal(t, edited, actual)
	finished, err := s.Store.StorageOperation(t.Context(), op.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), finished.CopiedObjects)
	require.Equal(t, entries[0].Size, finished.CopiedBytes)
	require.Equal(t, store.StorageOperationFailed, finished.State)
}

func TestCheckoutPausedRestart(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	a := checkoutPhoto(t, s, "photo.jpg", "bytes")
	c, op, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, t.TempDir(), 0)
	require.NoError(t, err)
	require.NoError(t, root.Close())
	control, err := s.Store.SetLaneControl(t.Context(), store.LaneControl{Lane: "photo_checkout", Paused: true, Concurrency: 1}, 1)
	require.NoError(t, err)
	supervisor := jobs.New(t.Context(), nil)
	require.NoError(t, s.Resume(t.Context(), supervisor))
	require.NoError(t, supervisor.Shutdown(context.Background()))
	started, err := s.Store.PhotoCheckoutStarted(t.Context(), c.ID)
	require.NoError(t, err)
	require.False(t, started)
	queued, err := s.Store.PhotoCheckout(t.Context(), c.ID)
	require.NoError(t, err)
	require.Equal(t, "queued", queued.State)
	supervisor = jobs.New(t.Context(), nil)
	defer func() { require.NoError(t, supervisor.Shutdown(context.Background())) }()
	require.NoError(t, s.Resume(t.Context(), supervisor))
	control.Paused = false
	_, err = s.Store.SetLaneControl(t.Context(), control, control.Revision)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		op, err := s.Store.StorageOperation(t.Context(), op.ID)
		return err == nil && op.State == store.StorageOperationCompleted
	}, 5*time.Second, 10*time.Millisecond)
}

func TestCheckoutResumeReconcilesStartedWorkInSupervisor(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	a := checkoutPhoto(t, s, "photo.jpg", "bytes")
	c, op, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, t.TempDir(), 0)
	require.NoError(t, err)
	require.NoError(t, root.Close())
	require.NoError(t, s.Store.ClaimPhotoCheckout(t.Context(), c.ID, op.ID))
	supervisor := jobs.New(t.Context(), nil)
	defer func() { require.NoError(t, supervisor.Shutdown(context.Background())) }()
	require.NoError(t, s.Resume(t.Context(), supervisor))
	require.Eventually(t, func() bool {
		actual, err := s.Store.PhotoCheckout(t.Context(), c.ID)
		return err == nil && actual.State == "failed" && strings.Contains(actual.Error, "interrupted")
	}, 5*time.Second, 10*time.Millisecond)
}

func TestCheckoutVisibilityChangesDuringCopy(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	for _, name := range []string{"first.jpg", "second.jpg", "third.jpg"} {
		checkoutPhoto(t, s, name, name)
	}
	c, _, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{All: true}, t.TempDir(), 100)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	entries, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
	require.NoError(t, err)
	blocked, err := s.Store.PhotoAssetByID(t.Context(), entries[1].AssetID)
	require.NoError(t, err)
	publications := 0
	s.Mutate = func(ctx context.Context, fn func() error) error {
		publications++
		if publications == 2 {
			_, err = s.Store.TrashPhotoAsset(ctx, blocked.ID, blocked.Revision)
			require.NoError(t, err)
		}
		return fn()
	}
	require.ErrorIs(t, s.Run(t.Context(), c, root), store.ErrInvalidPhotoCheckout)
	status, err := s.Store.PhotoCheckoutStatus(t.Context(), c.ID)
	require.NoError(t, err)
	require.Equal(t, int64(2), status.PublishedFiles)
	require.Equal(t, "failed", status.Checkout.State)
	require.Contains(t, status.Checkout.Error, entries[1].FileID)
	require.Contains(t, status.Checkout.Error, entries[1].Path)
	actual, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
	require.NoError(t, err)
	require.True(t, actual[0].Published)
	require.False(t, actual[1].Published)
	require.True(t, actual[2].Published)
	require.Equal(t, 3, publications)
}

func TestCheckoutAdmissionReleasesActivity(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	a := checkoutPhoto(t, s, "photo.jpg", "bytes")
	c, _, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, t.TempDir(), 0)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	inflight, admissions := 0, 0
	s.ActivityBegin = func() { inflight++ }
	s.ActivityEnd = func() { inflight-- }
	s.admit = func(context.Context, *store.Store, string) (bool, error) {
		admissions++
		require.Zero(t, inflight)
		return false, nil
	}
	s.Mutate = func(_ context.Context, fn func() error) error { require.Equal(t, 1, inflight); return fn() }
	require.NoError(t, s.Run(t.Context(), c, root))
	require.Equal(t, 2, admissions)
	require.Zero(t, inflight)
}

func TestCheckoutQueuedDestinationRevalidatedBeforeCopy(t *testing.T) {
	t.Parallel()
	for _, changed := range []string{"unchanged", "nonempty"} {
		t.Run(changed, func(t *testing.T) {
			s := checkoutService(t)
			a := checkoutPhoto(t, s, "photo.jpg", "bytes")
			selection := store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}
			c, _, root, err := s.Prepare(t.Context(), selection, t.TempDir(), 0)
			require.NoError(t, err)
			require.NoError(t, root.Close())
			if changed == "nonempty" {
				require.NoError(t, os.WriteFile(filepath.Join(c.Destination, "keep"), []byte("keep"), 0600))
			}
			err = s.Run(t.Context(), c, nil)
			status, statusErr := s.Store.PhotoCheckoutStatus(t.Context(), c.ID)
			require.NoError(t, statusErr)
			if changed == "unchanged" {
				require.NoError(t, err)
				require.Equal(t, "completed", status.Checkout.State)
				return
			}
			require.ErrorIs(t, err, store.ErrInvalidPhotoCheckout)
			require.Equal(t, "failed", status.Checkout.State)
			require.Zero(t, status.PublishedFiles)
			if changed == "nonempty" {
				content, err := os.ReadFile(filepath.Join(c.Destination, "keep"))
				require.NoError(t, err)
				require.Equal(t, "keep", string(content))
			}
		})
	}
}

func TestCheckoutAdmissionRetryKeepsVerifiedPartialCopies(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	checkoutPhoto(t, s, "one.jpg", "one")
	checkoutPhoto(t, s, "two.jpg", "two")
	c, _, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{All: true}, t.TempDir(), 100)
	require.NoError(t, err)
	require.NoError(t, root.Close())
	admissions := 0
	s.admit = func(context.Context, *store.Store, string) (bool, error) {
		admissions++
		if admissions == 3 {
			return false, store.ErrLaneControl
		}
		return false, nil
	}
	require.ErrorIs(t, s.Run(t.Context(), c, nil), jobs.ErrStorageOperationDeferred)
	partial, err := s.Store.PhotoCheckoutStatus(t.Context(), c.ID)
	require.NoError(t, err)
	require.Equal(t, "queued", partial.Checkout.State)
	require.Equal(t, int64(1), partial.PublishedFiles)
	require.NoError(t, s.Run(t.Context(), c, nil))
	complete, err := s.Store.PhotoCheckoutStatus(t.Context(), c.ID)
	require.NoError(t, err)
	require.Equal(t, "completed", complete.Checkout.State)
	require.Equal(t, int64(2), complete.PublishedFiles)
}
