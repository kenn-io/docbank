package ingest

import (
	"path/filepath"
	"testing"

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
}
