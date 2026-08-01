//go:build !windows

package backupapp_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/backupapp"
)

func TestLoadRestoreStoreMapRejectsBroadPermissionsAndSymlinks(t *testing.T) {
	content := []byte("version = 1\n")
	path := filepath.Join(t.TempDir(), "store-map.toml")
	require.NoError(t, os.WriteFile(path, content, 0o644))
	require.NoError(t, os.Chmod(path, 0o644))
	_, err := backupapp.LoadRestoreStoreMap(path)
	require.ErrorContains(t, err, "permissions must be 0600 or stricter")

	require.NoError(t, os.Chmod(path, 0o600))
	link := filepath.Join(t.TempDir(), "store-map-link.toml")
	require.NoError(t, os.Symlink(path, link))
	_, err = backupapp.LoadRestoreStoreMap(link)
	require.Error(t, err)
}
