//go:build unix

package backupapp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

func TestRejectedRestoreMappingDoesNotRepairFilesystemNamespace(t *testing.T) {
	namespace := filepath.Join(t.TempDir(), "archive")
	require.NoError(t, os.Mkdir(namespace, 0o700))
	require.NoError(t, os.Chmod(namespace, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(namespace, "operator-note.txt"), []byte("preserve"), 0o600,
	))

	_, err := claimRestoreBackend(
		t.Context(), "30000000-0000-4000-8000-000000000001",
		store.BlobStore{
			ID: "10000000-0000-4000-8000-000000000001", Name: "archive",
			OwnershipEpoch: "20000000-0000-4000-8000-000000000001",
		},
		config.StoreBindingConfig{Kind: "filesystem", Path: namespace},
		false, make(map[packstore.Ownership]string),
	)
	require.Error(t, err)
	info, statErr := os.Stat(namespace)
	require.NoError(t, statErr)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}
