package docbank_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/docbank"
)

func TestEmbeddedBackupReservedTag(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	repo, err := docbank.InitBackupRepository(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	_, err = vault.CreateBackup(t.Context(), repo, docbank.BackupOptions{Tag: "docbank:scheduled"})
	require.ErrorContains(t, err, "reserved")
	points, err := repo.Snapshots()
	require.NoError(t, err)
	require.Empty(t, points)
}
