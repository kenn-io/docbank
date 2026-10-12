package checkout

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
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
	require.NoError(t, s.Recover(t.Context()))
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

func TestCheckoutRejectsChangedVisibilityBeforePublication(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	a := checkoutPhoto(t, s, "photo.jpg", "bytes")
	c, _, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, t.TempDir(), 0)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	_, err = s.Store.SetPhotoAssetExcluded(t.Context(), a.ID, a.Revision, true)
	require.NoError(t, err)
	require.ErrorIs(t, s.Run(t.Context(), c, root), store.ErrInvalidPhotoCheckout)
	entries, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
	require.NoError(t, err)
	require.False(t, entries[0].Published)
	_, err = root.Stat(filepath.FromSlash(entries[0].Path))
	require.ErrorIs(t, err, os.ErrNotExist)
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
