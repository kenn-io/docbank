//go:build !windows

package checkout

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
	"os"
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
	require.NoError(t, s.Recover(t.Context()))
	status, err := s.Store.PhotoCheckout(t.Context(), c.ID)
	require.NoError(t, err)
	require.Equal(t, "failed", status.State)
	require.Contains(t, status.Error, "staging cleanup failed")
}
