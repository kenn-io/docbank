//go:build windows

package checkout

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/filepublish"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
	"golang.org/x/sys/windows"
)

func TestCheckoutRecoveryRetriesFilesystemAccessFailures(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		root      bool
		cancel    bool
		published bool
		stage     bool
	}{
		{"file", false, false, false, false},
		{"root", true, false, false, false},
		{"cancel while root inaccessible", true, true, false, false},
		{"published file", false, false, true, false},
		{"cancel while published file inaccessible", false, true, true, false},
		{"stage", false, false, false, true},
		{"cancel while stage inaccessible", false, true, false, true},
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
			require.NoError(t, s.Store.AdvanceStorageOperation(ctx, op.ID, entries[0].FileID, 0, 0, 0, "{}"))
			require.NoError(t, root.MkdirAll(filepath.Dir(filepath.FromSlash(entries[0].Path)), 0o700))
			require.NoError(t, root.WriteFile(filepath.FromSlash(entries[0].Path), []byte("verified bytes"), 0o600))
			if test.published {
				require.NoError(t, s.Store.PublishPhotoCheckoutEntry(ctx, entries[0], func() (bool, error) { return true, nil }))
			}
			lockedPath := filepath.Join(checkout.Destination, filepath.FromSlash(entries[0].Path))
			if test.stage {
				stage, err := filepublish.CreateRootStage(root)
				require.NoError(t, err)
				lockedPath = filepath.Join(checkout.Destination, filepath.Base(stage.File.Name()))
				require.NoError(t, stage.File.Close())
			}
			require.NoError(t, root.Close())
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
			if test.published {
				require.Equal(t, int64(1), status.PublishedFiles)
			} else {
				require.Zero(t, status.PublishedFiles)
			}
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
			if !test.cancel {
				require.NoError(t, windows.CloseHandle(handle))
				handle = windows.InvalidHandle
			}
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
			if !test.cancel || test.published {
				require.Equal(t, int64(1), status.PublishedFiles)
			} else {
				require.Zero(t, status.PublishedFiles)
			}
			if test.stage && !test.cancel {
				_, err := os.Stat(lockedPath)
				require.ErrorIs(t, err, os.ErrNotExist)
			}
			held, err = s.Store.PruneContentVersions(ctx, node.ID, updated.Revision, selector, true)
			require.NoError(t, err)
			require.Len(t, held.DependencyRetained, 1)
			if test.cancel {
				retired, err := s.Store.RetirePhotoCheckout(ctx, checkout.ID)
				require.NoError(t, err)
				require.NotNil(t, retired.RetiredAt)
				pruned, err := s.Store.PruneContentVersions(ctx, node.ID, updated.Revision, selector, true)
				require.NoError(t, err)
				require.Equal(t, 1, pruned.DeletedVersions)
			}
		})
	}
}
