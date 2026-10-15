//go:build windows

package checkout

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
	"golang.org/x/sys/windows"
)

func TestCheckoutRecoveryRetainsUnreadableUnrecordedInstall(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	ctx := t.Context()
	asset := checkoutPhoto(t, s, "photo.jpg", "verified bytes")
	checkout, op, root, err := s.Prepare(ctx, store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, t.TempDir(), 0)
	require.NoError(t, err)
	defer func() { require.NoError(t, root.Close()) }()
	require.NoError(t, s.Store.ClaimPhotoCheckout(ctx, checkout.ID, op.ID))
	entries, err := s.Store.PhotoCheckoutEntries(ctx, checkout.ID)
	require.NoError(t, err)
	// Model a crash after install but before its publication record is committed.
	require.NoError(t, root.MkdirAll(filepath.Dir(filepath.FromSlash(entries[0].Path)), 0o700))
	require.NoError(t, root.WriteFile(filepath.FromSlash(entries[0].Path), []byte("verified bytes"), 0o600))
	name, err := windows.UTF16PtrFromString(filepath.Join(checkout.Destination, filepath.FromSlash(entries[0].Path)))
	require.NoError(t, err)
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	require.NoError(t, err)
	t.Cleanup(func() {
		if handle != windows.InvalidHandle {
			require.NoError(t, windows.CloseHandle(handle))
		}
	})
	require.Error(t, s.reconcile(ctx, checkout))
	operation, err := s.Store.StorageOperation(ctx, op.ID)
	require.NoError(t, err)
	require.Equal(t, store.StorageOperationRunning, operation.State)
	require.Nil(t, operation.FinishedAt)
	status, err := s.Store.PhotoCheckoutStatus(ctx, checkout.ID)
	require.NoError(t, err)
	require.False(t, status.Checkout.RetentionReleased)
	require.Zero(t, status.PublishedFiles)
	node, err := s.Store.NodeByID(ctx, asset.Files[0].NodeID)
	require.NoError(t, err)
	hash, size, err := s.Blobs.Write(strings.NewReader("replacement"))
	require.NoError(t, err)
	updated, _, err := s.Store.ReplaceContent(ctx, node.ID, node.Revision, hash, size, "image/jpeg")
	require.NoError(t, err)
	selector := store.VersionPruneSelector{VersionIDs: []string{entries[0].VersionID}}
	held, err := s.Store.PruneContentVersions(ctx, node.ID, updated.Revision, selector, true)
	require.NoError(t, err)
	require.Len(t, held.DependencyRetained, 1)
	require.NoError(t, windows.CloseHandle(handle))
	handle = windows.InvalidHandle
	require.NoError(t, s.reconcile(ctx, checkout))
	status, err = s.Store.PhotoCheckoutStatus(ctx, checkout.ID)
	require.NoError(t, err)
	require.Equal(t, "failed", status.Checkout.State)
	require.Equal(t, int64(1), status.PublishedFiles)
	held, err = s.Store.PruneContentVersions(ctx, node.ID, updated.Revision, selector, true)
	require.NoError(t, err)
	require.Len(t, held.DependencyRetained, 1)
}
