//go:build windows

package checkout

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
	"golang.org/x/sys/windows"
)

func TestCheckoutRecoveryRetainsUnreadableUnrecordedInstall(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		root   bool
		cancel bool
	}{
		{"file", false, false},
		{"root", true, false},
		{"cancel while root inaccessible", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			s := checkoutService(t)
			ctx := t.Context()
			asset := checkoutPhoto(t, s, "photo.jpg", "verified bytes")
			checkout, op, root, err := s.Prepare(ctx, store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, t.TempDir(), 0)
			require.NoError(t, err)
			require.NoError(t, s.Store.ClaimPhotoCheckout(ctx, checkout.ID, op.ID))
			entries, err := s.Store.PhotoCheckoutEntries(ctx, checkout.ID)
			require.NoError(t, err)
			// Model a crash after install but before its publication record is committed.
			require.NoError(t, root.MkdirAll(filepath.Dir(filepath.FromSlash(entries[0].Path)), 0o700))
			require.NoError(t, root.WriteFile(filepath.FromSlash(entries[0].Path), []byte("verified bytes"), 0o600))
			require.NoError(t, root.Close())
			lockedPath := filepath.Join(checkout.Destination, filepath.FromSlash(entries[0].Path))
			flags := uint32(windows.FILE_ATTRIBUTE_NORMAL)
			if test.root {
				lockedPath = checkout.Destination
				flags = windows.FILE_FLAG_BACKUP_SEMANTICS
			}
			name, err := windows.UTF16PtrFromString(lockedPath)
			require.NoError(t, err)
			handle, err := windows.CreateFile(name, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, flags, 0)
			require.NoError(t, err)
			t.Cleanup(func() {
				if handle != windows.InvalidHandle {
					require.NoError(t, windows.CloseHandle(handle))
				}
			})
			supervisor := jobs.New(ctx, nil)
			defer func() { require.NoError(t, supervisor.Shutdown(context.Background())) }()
			require.NoError(t, s.Resume(ctx, supervisor))
			require.Eventually(t, func() bool {
				operation, err := s.Store.StorageOperation(ctx, op.ID)
				return err == nil && operation.State == store.StorageOperationQueued && operation.Error != ""
			}, 5*time.Second, 10*time.Millisecond)
			operation, err := s.Store.StorageOperation(ctx, op.ID)
			require.NoError(t, err)
			require.Equal(t, store.StorageOperationQueued, operation.State)
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
			if test.cancel {
				require.NoError(t, s.Store.RequestStorageOperationCancel(ctx, op.ID))
			}
			require.NoError(t, windows.CloseHandle(handle))
			handle = windows.InvalidHandle
			require.Eventually(t, func() bool {
				operation, err := s.Store.StorageOperation(ctx, op.ID)
				return err == nil && operation.FinishedAt != nil
			}, 5*time.Second, 10*time.Millisecond)
			status, err = s.Store.PhotoCheckoutStatus(ctx, checkout.ID)
			require.NoError(t, err)
			wantState := "failed"
			if test.cancel {
				wantState = "cancelled"
			}
			require.Equal(t, wantState, status.Checkout.State)
			require.Equal(t, int64(1), status.PublishedFiles)
			held, err = s.Store.PruneContentVersions(ctx, node.ID, updated.Revision, selector, true)
			require.NoError(t, err)
			require.Len(t, held.DependencyRetained, 1)
		})
	}
}
