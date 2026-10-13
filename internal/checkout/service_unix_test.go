//go:build !windows

package checkout

import (
	"context"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckoutRecoveryContinuesAfterDestinationBecomesInaccessible(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	asset := checkoutPhoto(t, s, "photo.jpg", "bytes")
	selection := store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}
	c, _, root, err := s.Prepare(t.Context(), selection, t.TempDir(), 0)
	require.NoError(t, err)
	require.NoError(t, root.Close())
	require.NoError(t, os.Chmod(c.Destination, 0))
	defer func() { require.NoError(t, os.Chmod(c.Destination, 0o700)) }()
	probe, err := os.OpenRoot(c.Destination)
	if err == nil {
		_ = probe.Close()
		t.Skip("this user can open directories without read permission")
	}
	require.NoError(t, s.reconcile(t.Context(), c))
	status, err := s.Store.PhotoCheckout(t.Context(), c.ID)
	require.NoError(t, err)
	require.Equal(t, "failed", status.State)
	require.Contains(t, status.Error, "reconciliation failed")
}

func TestCheckoutMovedAssetDirectoryCannotReceivePublication(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	a := checkoutPhoto(t, s, "photo.jpg", "bytes")
	c, _, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, t.TempDir(), 0)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	entries, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
	require.NoError(t, err)
	outside := filepath.Join(t.TempDir(), "moved")
	s.Mutate = func(_ context.Context, fn func() error) error {
		parent := filepath.Join(c.Destination, filepath.Dir(filepath.FromSlash(entries[0].Path)))
		require.NoError(t, os.Rename(parent, outside))
		require.NoError(t, os.Symlink(outside, parent))
		return fn()
	}
	require.Error(t, s.Run(t.Context(), c, root))
	_, err = os.Stat(filepath.Join(outside, "photo.jpg"))
	require.ErrorIs(t, err, os.ErrNotExist)
	status, err := s.Store.PhotoCheckoutStatus(t.Context(), c.ID)
	require.NoError(t, err)
	require.Equal(t, "failed", status.Checkout.State)
	require.Zero(t, status.PublishedFiles)
}
