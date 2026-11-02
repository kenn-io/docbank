//go:build darwin || linux

package safefileio

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVerifyPrivateFileModeRejectsPublicMode(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "record.json")
	require.NoError(os.WriteFile(path, []byte("{}"), 0o600))
	require.NoError(os.Chmod(path, 0o666))
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(err)
	defer func() { _ = file.Close() }()

	require.ErrorContains(verifyPrivateFileMode(file), "mode is 0666, not 0600")
}
