//go:build windows

package home

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/winsecurity"
	"go.kenn.io/kit/safefileio"
)

func TestEnsureCreatesPrivateWindowsLayout(t *testing.T) {
	layout := Layout{Root: t.TempDir()}
	configPath := filepath.Join(layout.Root, "config.toml")
	require.NoError(t, os.WriteFile(configPath, []byte("[server]\napi_key = \"private\"\n"), 0o600))
	require.NoError(t, layout.Ensure())
	for _, dir := range []string{
		layout.Root, layout.BlobsDir(), layout.BlobTmpDir(), layout.LogsDir(),
	} {
		require.NoError(t, safefileio.ValidatePrivateDir(dir))
	}
	database, err := safefileio.OpenCurrentUserFile(layout.DBPath())
	require.NoError(t, err)
	require.NoError(t, database.Close())
	configFile, err := winsecurity.OpenRestrictedCurrentUserFile(configPath)
	require.NoError(t, err)
	require.NoError(t, configFile.Close())
}

func TestOpenAndLockExclusiveCreatesWindowsDirectoriesPrivateAtomically(t *testing.T) {
	target := filepath.Join(t.TempDir(), "first", "second", "vault")
	seen := 0
	root, lock, err := (Layout{Root: target}).createAndLockExclusiveWith(
		nil,
		func(_ int, created *os.Root) error {
			seen++
			return safefileio.ValidatePrivateDir(created.Name())
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })
	t.Cleanup(func() { require.NoError(t, lock.Release()) })
	require.Equal(t, 3, seen)
}
