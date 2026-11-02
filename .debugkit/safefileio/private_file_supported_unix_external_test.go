//go:build darwin || linux

package safefileio_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
)

func TestValidatePrivateCurrentUserFileRejectsPublicMode(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "record.json")
	require.NoError(os.WriteFile(path, []byte("{}"), 0o600))
	require.NoError(os.Chmod(path, 0o666))
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(err)
	defer func() { _ = file.Close() }()

	require.Error(safefileio.ValidatePrivateCurrentUserFile(file))
	info, err := file.Stat()
	require.NoError(err)
	require.Equal(os.FileMode(0o666), info.Mode().Perm())
}

func TestValidatePrivateCurrentUserFileAcceptsPrivateMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record.json")
	require.NoError(t, os.WriteFile(path, []byte("{}"), 0o600))
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	defer func() { _ = file.Close() }()

	require.NoError(t, safefileio.ValidatePrivateCurrentUserFile(file))
}
