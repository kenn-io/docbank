package ingest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func TestCheckoutPathOverlapPrioritizesConfirmedReservation(t *testing.T) {
	unavailable := store.PhotoCheckout{ID: "unavailable", Destination: filepath.Join(t.TempDir(), "invalid\x00path")}
	reserved := store.PhotoCheckout{ID: "reserved", Destination: t.TempDir()}
	for _, checkouts := range [][]store.PhotoCheckout{{unavailable, reserved}, {reserved, unavailable}} {
		id, overlap, _, err := CheckoutPathOverlap(t.Context(), checkouts, filepath.Join(reserved.Destination, "child"))
		require.NoError(t, err)
		require.True(t, overlap)
		require.Equal(t, reserved.ID, id)
	}
	_, overlap, _, err := CheckoutPathOverlap(t.Context(), []store.PhotoCheckout{unavailable}, t.TempDir())
	require.Error(t, err, "a binding with only inconclusive checks must stay closed")
	require.False(t, overlap)

	for _, state := range []store.StorageOperationState{store.StorageOperationQueued, store.StorageOperationFailed} {
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
			if state == store.StorageOperationFailed {
				require.NoError(t, ing.Store.FinishPhotoCheckout(ctx, checkout.ID, op.ID, state, "{}", "", time.Now().Add(time.Hour)))
			}
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
			check(destination, state == store.StorageOperationQueued)
			partial := filepath.Join(destination, "photo.jpg")
			require.NoError(t, os.WriteFile(partial, []byte("partial"), 0o600))
			check(destination, true)
			require.NoError(t, os.Remove(partial))
			check(destination, state == store.StorageOperationQueued)
			require.NoError(t, os.Remove(destination))
			check(destination, state == store.StorageOperationQueued)
			check(t.TempDir(), false)
			require.NoError(t, os.Mkdir(destination, 0o700))
			edited := filepath.Join(destination, "edited.jpg")
			require.NoError(t, os.WriteFile(edited, []byte("edits"), 0o600))
			if state == store.StorageOperationQueued {
				require.NoError(t, ing.Store.FinishPhotoCheckout(ctx, checkout.ID, op.ID, store.StorageOperationCancelled, "{}", "cancelled", time.Now().Add(time.Hour)))
			}
			_, err = ing.Store.RetirePhotoCheckout(ctx, checkout.ID)
			require.NoError(t, err)
			check(destination, true)
			require.NoError(t, os.Remove(edited))
			check(destination, false)
		})
	}
}
