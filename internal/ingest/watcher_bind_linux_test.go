package ingest

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

func TestWatcherRejectsVaultHiddenBySeparateBindAliases(t *testing.T) {
	backing := t.TempDir()
	vault := filepath.Join(backing, "private", "vault")
	require.NoError(t, os.MkdirAll(vault, 0o700))
	aliases := t.TempDir()
	sourceAlias := filepath.Join(aliases, "source")
	vaultAlias := filepath.Join(aliases, "vault")
	require.NoError(t, os.Mkdir(sourceAlias, 0o700))
	require.NoError(t, os.Mkdir(vaultAlias, 0o700))

	bindMountForTest(t, backing, sourceAlias)
	bindMountForTest(t, vault, vaultAlias)
	require.False(t, pathContains(sourceAlias, vaultAlias),
		"separate bind aliases must hide lexical ancestry")

	watcher, err := NewWatcher(newTestIngester(t), vaultAlias, config.WatchConfig{
		Name: "bind-alias", Source: sourceAlias, Destination: "/inbox",
		SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Second),
	}, runTestMutation, nil)
	require.NoError(t, err)
	_, err = watcher.openRoot(t.Context())
	require.ErrorContains(t, err, "contains vault storage through a filesystem alias")
}

func TestWatcherRejectsBindAliasToVaultDescendantAsSource(t *testing.T) {
	vault := t.TempDir()
	logs := filepath.Join(vault, "logs")
	require.NoError(t, os.Mkdir(logs, 0o700))
	aliases := t.TempDir()
	sourceAlias := filepath.Join(aliases, "source")
	require.NoError(t, os.Mkdir(sourceAlias, 0o700))
	bindMountForTest(t, logs, sourceAlias)

	watcher, err := NewWatcher(newTestIngester(t), vault, config.WatchConfig{
		Name: "logs-alias", Source: sourceAlias, Destination: "/inbox",
		SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Second),
	}, runTestMutation, nil)
	require.NoError(t, err)
	_, err = watcher.openRoot(t.Context())
	require.ErrorContains(t, err, "contains vault storage through a filesystem alias")
}

func TestWatchBindingOverlapRejectsMissingPathThroughBindAliasToDescendant(t *testing.T) {
	watchSource := t.TempDir()
	protected := filepath.Join(watchSource, "documents")
	require.NoError(t, os.Mkdir(protected, 0o700))
	alias := filepath.Join(t.TempDir(), "documents-alias")
	require.NoError(t, os.Mkdir(alias, 0o700))
	bindMountForTest(t, protected, alias)
	bindingPath := filepath.Join(alias, "archive")
	require.False(t, pathContains(watchSource, bindingPath),
		"the bind alias must hide lexical ancestry")

	watchName, overlaps, err := WatchBindingOverlap(
		[]config.WatchConfig{{Name: "inbox", Source: watchSource}},
		config.StoreBindingConfig{Kind: "filesystem", Path: bindingPath},
	)
	require.NoError(t, err)
	require.Equal(t, "inbox", watchName)
	require.True(t, overlaps)
}

func bindMountForTest(t *testing.T, source, target string) {
	t.Helper()
	err := unix.Mount(source, target, "", unix.MS_BIND, "")
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
		t.Skipf("bind mounts are not permitted in this test environment: %v", err)
	}
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, unix.Unmount(target, unix.MNT_DETACH))
	})
}

// One traversal of the configured tree covers every retired checkout, and a
// match still names the checkout whose copies the alias exposes.
func TestCheckoutPathOverlapFindsAliasAmongRetiredCheckouts(t *testing.T) {
	var checkouts []store.PhotoCheckout
	for _, id := range []string{"first", "second", "third"} {
		destination := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(destination, "edited.jpg"), []byte(id), 0o600))
		checkouts = append(checkouts, store.PhotoCheckout{ID: id, Destination: destination, RetentionReleased: true})
	}
	watchSource := t.TempDir()
	alias := filepath.Join(watchSource, "nested", "alias")
	require.NoError(t, os.MkdirAll(alias, 0o700))
	bindMountForTest(t, checkouts[1].Destination, alias)

	id, overlap, err := CheckoutPathOverlap(t.Context(), checkouts, watchSource)
	require.NoError(t, err)
	require.True(t, overlap)
	require.Equal(t, "second", id)
	related, err := RelatedCheckouts(t.Context(), checkouts, watchSource)
	require.NoError(t, err)
	require.Equal(t, []store.PhotoCheckout{checkouts[1]}, related)
}
