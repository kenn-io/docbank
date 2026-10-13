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
	for _, kind := range []string{"watch", "vault", "storage", "checkout"} {
		t.Run(kind, func(t *testing.T) {
			s := checkoutService(t)
			asset := checkoutPhoto(t, s, "photo.jpg", "bytes")
			selection := store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}
			source := t.TempDir()
			switch kind {
			case "watch":
				s.Config.Watches = []config.WatchConfig{{Name: "inbox", Source: source}}
			case "vault":
				source = s.VaultRoot
			case "storage":
				s.Config.StoreBindings = map[string]config.StoreBindingConfig{"archive": {Kind: "filesystem", Path: source}}
			case "checkout":
				_, _, root, err := s.Prepare(t.Context(), selection, source, 0)
				require.NoError(t, err)
				require.NoError(t, root.Close())
			}
			protected := filepath.Join(source, "photos")
			require.NoError(t, os.Mkdir(protected, 0700))
			alias := t.TempDir()
			bindCheckoutAlias(t, protected, alias)
			_, _, root, err := s.Prepare(t.Context(), selection, alias, 0)
			if root != nil {
				_ = root.Close()
			}
			require.ErrorIs(t, err, store.ErrInvalidPhotoCheckout)
		})
	}
}

func TestCheckoutQueuedResumeRejectsCurrentWatchBindAlias(t *testing.T) {
	s := checkoutService(t)
	asset := checkoutPhoto(t, s, "photo.jpg", "bytes")
	selection := store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}
	c, _, root, err := s.Prepare(t.Context(), selection, t.TempDir(), 0)
	require.NoError(t, err)
	require.NoError(t, root.Close())
	source := t.TempDir()
	protected := filepath.Join(source, "photos")
	require.NoError(t, os.Mkdir(protected, 0700))
	bindCheckoutAlias(t, protected, c.Destination)
	s.Config.Watches = []config.WatchConfig{{Name: "inbox", Source: source}}
	require.ErrorIs(t, s.Run(t.Context(), c, nil), store.ErrInvalidPhotoCheckout)
	status, err := s.Store.PhotoCheckoutStatus(t.Context(), c.ID)
	require.NoError(t, err)
	require.Equal(t, "failed", status.Checkout.State)
	require.Zero(t, status.PublishedFiles)
	files, err := os.ReadDir(protected)
	require.NoError(t, err)
	require.Empty(t, files)
}
