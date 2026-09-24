package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWebDownloadStagingInitializationDoesNotErasePreparedPackage(t *testing.T) {
	root := t.TempDir()
	downloads := newWebDownloadRegistry(root)
	require.NoError(t, os.Mkdir(downloads.dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(downloads.dir, "abandoned"), []byte("old"), 0o600))
	require.NoError(t, downloads.ensureStagingDir())
	_, err := os.Stat(filepath.Join(downloads.dir, "abandoned"))
	require.ErrorIs(t, err, os.ErrNotExist)

	packageDir := filepath.Join(downloads.dir, "prepared-package")
	require.NoError(t, os.Mkdir(packageDir, 0o700))
	packagePath := filepath.Join(packageDir, "recipient.zip")
	require.NoError(t, os.WriteFile(packagePath, []byte("synthetic staged package"), 0o600))
	file, stagedPath, err := downloads.createStagingFile()
	require.NoError(t, err)
	require.NoError(t, file.Close())
	require.NoError(t, os.Remove(stagedPath))
	require.NoError(t, downloads.ensureStagingDir())
	data, err := os.ReadFile(packagePath)
	require.NoError(t, err)
	require.Equal(t, "synthetic staged package", string(data))
}
