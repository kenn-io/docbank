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

func bindCheckoutAlias(t *testing.T, source, alias string) {
	t.Helper()
	err := unix.Mount(source, alias, "", unix.MS_BIND, "")
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
		t.Skipf("bind mounts unavailable: %v", err)
	}
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, unix.Unmount(alias, unix.MNT_DETACH)) })
}

func TestCheckoutRejectsBindAliasIntoProtectedFolder(t *testing.T) {
	s := checkoutService(t)
	asset := checkoutPhoto(t, s, "photo.jpg", "bytes")
	selection := store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}
	source := t.TempDir()
	s.Config.Watches = []config.WatchConfig{{Name: "inbox", Source: source}}
	protected := filepath.Join(source, "photos")
	require.NoError(t, os.Mkdir(protected, 0700))
	alias := t.TempDir()
	bindCheckoutAlias(t, protected, alias)
	_, _, root, err := s.Prepare(t.Context(), selection, alias, 0)
	if root != nil {
		_ = root.Close()
	}
	require.ErrorIs(t, err, store.ErrInvalidPhotoCheckout)
}
