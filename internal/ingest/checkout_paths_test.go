package ingest

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/config"
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
			parent := filepath.Join(t.TempDir(), "parent")
			destination := filepath.Join(parent, "checkout")
			require.NoError(t, os.MkdirAll(destination, 0o700))
			checkout, op, _, err := ing.Store.CreatePhotoCheckout(ctx, store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, destination, "", 0)
			require.NoError(t, err)
			require.NoError(t, ing.Store.FinishPhotoCheckout(ctx, checkout.ID, op.ID, state, "{}", "", time.Now().Add(time.Hour)))
			check := func(path string, want bool) {
				t.Helper()
				checkouts, err := ing.Store.ListPhotoCheckouts(ctx)
				require.NoError(t, err)
				id, overlaps, err := CheckoutPathOverlap(ctx, checkouts, path)
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
				require.NoError(t, ing.Store.BindPhotoCheckout(ctx, checkout.ID, destination, "", "", nil))
				commit, _, err := ing.Store.PreparePhotoCheckoutOperation(ctx, store.PhotoCheckoutInspection{CheckoutID: checkout.ID}, store.StorageOperationKindPhotoCheckoutCommit)
				require.NoError(t, err)
				check(destination, true)
				require.NoError(t, ing.Store.FinishPhotoCheckoutOperation(ctx, commit, store.StorageOperationCancelled, "{}", ""))
				check(destination, true)
				return
			}
			require.NoError(t, os.Mkdir(destination, 0o700))
			edited := filepath.Join(destination, "edited.jpg")
			require.NoError(t, os.WriteFile(edited, []byte("edits"), 0o600))
			_, err = ing.Store.RetirePhotoCheckout(ctx, checkout.ID)
			require.NoError(t, err)
			check(destination, true)
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
			require.NoError(t, os.Remove(parent))
			require.NoError(t, os.WriteFile(parent, []byte("replacement"), 0o600))
			check(destination, false)
			check(t.TempDir(), false)
		})
	}
	t.Run("unreadable retired destination", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("requires Unix directory permissions")
		}
		ing := newTestIngester(t)
		ctx := t.Context()
		node, err := ing.Store.CreateFile(ctx, ing.Store.RootID(), "photo.jpg", strings.Repeat("a", 64), 20, "image/jpeg")
		require.NoError(t, err)
		asset, err := ing.Store.PhotoAssetForNode(ctx, node.ID)
		require.NoError(t, err)
		parent := filepath.Join(t.TempDir(), "parent")
		destination := filepath.Join(parent, "checkout")
		require.NoError(t, os.MkdirAll(destination, 0o700))
		checkout, op, _, err := ing.Store.CreatePhotoCheckout(ctx, store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, destination, "", 0)
		require.NoError(t, err)
		require.NoError(t, ing.Store.FinishPhotoCheckout(ctx, checkout.ID, op.ID, store.StorageOperationCompleted, "{}", "", time.Now().Add(time.Hour)))
		_, err = ing.Store.RetirePhotoCheckout(ctx, checkout.ID)
		require.NoError(t, err)
		checkouts, err := ing.Store.ListPhotoCheckouts(ctx)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, os.Chmod(parent, 0o700)) })
		require.NoError(t, os.Chmod(parent, 0o000))
		id, overlap, err := CheckoutPathOverlap(ctx, checkouts, t.TempDir())
		require.ErrorIs(t, err, os.ErrPermission)
		require.Equal(t, checkout.ID, id)
		require.False(t, overlap)
		for _, related := range []string{destination, filepath.Dir(destination), filepath.Join(destination, "nested")} {
			id, overlap, err := CheckoutPathOverlap(t.Context(), checkouts, related)
			require.NoError(t, err)
			require.True(t, overlap)
			require.Equal(t, checkout.ID, id)
		}
	})
}

func TestCheckoutPathOverlapWithReplacedAncestor(t *testing.T) {
	ing := newTestIngester(t)
	ctx := t.Context()
	node, err := ing.Store.CreateFile(ctx, ing.Store.RootID(), "photo.jpg", strings.Repeat("a", 64), 20, "image/jpeg")
	require.NoError(t, err)
	asset, err := ing.Store.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	parent := filepath.Join(t.TempDir(), "parent")
	destination := filepath.Join(parent, "checkout")
	require.NoError(t, os.MkdirAll(destination, 0o700))
	checkout, op, _, err := ing.Store.CreatePhotoCheckout(ctx, store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, destination, "", 0)
	require.NoError(t, err)
	require.NoError(t, ing.Store.FinishPhotoCheckout(ctx, checkout.ID, op.ID, store.StorageOperationCompleted, "{}", "", time.Now().Add(time.Hour)))
	require.NoError(t, os.Remove(destination))
	require.NoError(t, os.Remove(parent))
	require.NoError(t, os.WriteFile(parent, []byte("replacement"), 0o600))
	checkouts, err := ing.Store.ListPhotoCheckouts(ctx)
	require.NoError(t, err)
	require.False(t, checkouts[0].RetentionReleased)
	id, overlap, err := CheckoutPathOverlap(ctx, checkouts, t.TempDir())
	require.NoError(t, err)
	require.False(t, overlap)
	require.Empty(t, id)
	for _, path := range []string{parent, destination, filepath.Join(destination, "nested")} {
		id, overlap, err := CheckoutPathOverlap(ctx, checkouts, path)
		require.NoError(t, err)
		require.True(t, overlap)
		require.Equal(t, checkout.ID, id)
	}
}

// A folder reused after several retired checkouts releases every one of them,
// and leaves unrelated checkouts recorded.
func TestWatcherReleasesEveryRetiredCheckoutOfReusedFolder(t *testing.T) {
	ing := newTestIngester(t)
	ctx := t.Context()
	node, err := ing.Store.CreateFile(ctx, ing.Store.RootID(), "photo.jpg", strings.Repeat("a", 64), 20, "image/jpeg")
	require.NoError(t, err)
	asset, err := ing.Store.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	reused, unrelated := t.TempDir(), t.TempDir()
	retire := func(destination string) store.PhotoCheckout {
		checkout, op, _, err := ing.Store.CreatePhotoCheckout(ctx, store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, destination, "", 0)
		require.NoError(t, err)
		require.NoError(t, ing.Store.FinishPhotoCheckout(ctx, checkout.ID, op.ID, store.StorageOperationCompleted, "{}", "", time.Now().Add(time.Hour)))
		checkout, err = ing.Store.RetirePhotoCheckout(ctx, checkout.ID)
		require.NoError(t, err)
		return checkout
	}
	first, second, other := retire(reused), retire(reused), retire(unrelated)

	watcher, err := NewWatcher(ing, t.TempDir(), config.WatchConfig{Name: "inbox", Source: reused, Destination: "/inbox"}, runTestMutation, nil)
	require.NoError(t, err)
	root, err := watcher.openRoot(ctx)
	require.NoError(t, err)
	require.NoError(t, root.Close())

	for _, tc := range []struct {
		checkout    store.PhotoCheckout
		destination string
	}{{first, ""}, {second, ""}, {other, unrelated}} {
		current, err := ing.Store.PhotoCheckout(ctx, tc.checkout.ID)
		require.NoError(t, err)
		require.Equal(t, tc.destination, current.Destination)
	}
}
