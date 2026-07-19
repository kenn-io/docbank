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
