//go:build !windows

package web

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWriteBootstrapAppliesOwnerOnlyUnixModes(t *testing.T) {
	root := t.TempDir()
	_, err := WriteBootstrap(root, "http://127.0.0.1:43210/#api_key=private")
	require.NoError(t, err)
	dir := filepath.Join(root, launchDirName)
	dirInfo, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())
	fileInfo, err := os.Stat(filepath.Join(dir, "index.html"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), fileInfo.Mode().Perm())
}
