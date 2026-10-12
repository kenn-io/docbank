package checkout

import (
	"errors"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckoutRejectsBindAliasIntoWatchedFolder(t *testing.T) {
	s := checkoutService(t)
	asset := checkoutPhoto(t, s, "photo.jpg", "bytes")
	source := t.TempDir()
	protected := filepath.Join(source, "photos")
	require.NoError(t, os.Mkdir(protected, 0o700))
	alias := t.TempDir()
	err := unix.Mount(protected, alias, "", unix.MS_BIND, "")
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
		t.Skipf("bind mounts unavailable: %v", err)
	}
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, unix.Unmount(alias, unix.MNT_DETACH)) })
	s.Config.Watches = []config.WatchConfig{{Name: "inbox", Source: source}}
	_, _, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, alias, 0)
	if root != nil {
		_ = root.Close()
	}
	require.ErrorIs(t, err, store.ErrInvalidPhotoCheckout)
	checkouts, err := s.Store.ListPhotoCheckouts(t.Context())
	require.NoError(t, err)
	require.Empty(t, checkouts)
}
