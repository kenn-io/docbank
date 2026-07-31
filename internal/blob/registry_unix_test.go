//go:build unix

package blob

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
)

func TestFilesystemBackendSecuresExistingNamespaceDirectories(t *testing.T) {
	root := t.TempDir()
	shard := filepath.Join(root, "aa")
	require.NoError(t, os.Chmod(root, 0o755))
	require.NoError(t, os.Mkdir(shard, 0o755))

	_, err := NewFilesystemBackend(root, nil)
	require.Error(t, err)
	require.NoError(t, EnsureFilesystemNamespace(root))
	backend, err := NewFilesystemBackend(root, nil)
	require.NoError(t, err)
	require.NoError(t, backend.Close())
	require.NoError(t, safefileio.ValidatePrivateDir(root))
	require.NoError(t, safefileio.ValidatePrivateDir(shard))
}
