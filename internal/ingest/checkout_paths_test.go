package ingest

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func TestCheckoutPathOverlapProtectsReservationsAndReleasedCopies(t *testing.T) {
	for _, state := range []store.StorageOperationState{store.StorageOperationFailed, store.StorageOperationCompleted} {
		t.Run(string(state), func(t *testing.T) {
			ing := newTestIngester(t)
			ctx := t.Context()
			node, err := ing.Store.CreateFile(ctx, ing.Store.RootID(), "photo.jpg", strings.Repeat("a", 64), 20, "image/jpeg")
			require.NoError(t, err)
			asset, err := ing.Store.PhotoAssetForNode(ctx, node.ID)
			require.NoError(t, err)
			destination := t.TempDir()
			checkout, op, err := ing.Store.CreatePhotoCheckout(ctx, store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, destination, 0)
			require.NoError(t, err)
			require.NoError(t, ing.Store.FinishPhotoCheckout(ctx, checkout.ID, op.ID, state, "{}", "", time.Now().Add(time.Hour)))
			check := func(path string, want bool) {
				t.Helper()
				checkouts, err := ing.Store.ListPhotoCheckouts(ctx)
				require.NoError(t, err)
				id, overlaps, _, err := CheckoutPathOverlap(ctx, checkouts, path)
				require.NoError(t, err)
				require.Equal(t, want, overlaps)
				if want {
					require.Equal(t, checkout.ID, id)
				}
			}
			check(destination, state != store.StorageOperationFailed)
			partial := filepath.Join(destination, "photo.jpg")
			require.NoError(t, os.WriteFile(partial, []byte("partial"), 0o600))
			check(destination, true)
			require.NoError(t, os.Remove(partial))
			require.NoError(t, os.Remove(destination))
			check(destination, state != store.StorageOperationFailed)
			check(t.TempDir(), false)
			if state != store.StorageOperationCompleted {
				return
			}
			require.NoError(t, os.Mkdir(destination, 0o700))
			edited := filepath.Join(destination, "edited.jpg")
			require.NoError(t, os.WriteFile(edited, []byte("edits"), 0o600))
			_, err = ing.Store.RetirePhotoCheckout(ctx, checkout.ID)
			require.NoError(t, err)
			check(destination, true)
			if runtime.GOOS != "windows" && os.Geteuid() != 0 {
				require.NoError(t, os.Chmod(destination, 0o000))
				t.Cleanup(func() { _ = os.Chmod(destination, 0o700) })
				checkouts, err := ing.Store.ListPhotoCheckouts(ctx)
				require.NoError(t, err)
				_, _, _, err = CheckoutPathOverlap(ctx, checkouts, t.TempDir())
				require.ErrorIs(t, err, os.ErrPermission)
				require.NoError(t, os.Chmod(destination, 0o700))
			}
			require.NoError(t, os.Remove(edited))
			check(destination, false)

			// Reusing a retired checkout's former destination for an
			// unrelated regular file must not block admission at that
			// path: the checkout no longer holds anything there.
			require.NoError(t, os.Remove(destination))
			require.NoError(t, os.WriteFile(destination, []byte("unrelated"), 0o600))
			check(destination, false)
			require.NoError(t, os.Remove(destination))

			if runtime.GOOS != "windows" && os.Geteuid() != 0 {
				// The replacement file's own permissions must not matter:
				// the type check runs before anything tries to read it.
				require.NoError(t, os.WriteFile(destination, []byte("unreadable"), 0o000))
				check(destination, false)
				require.NoError(t, os.Chmod(destination, 0o600))
				require.NoError(t, os.Remove(destination))
			}
		})
	}
}
