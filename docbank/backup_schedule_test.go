package docbank_test

import (
	"os"
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
	_, err = vault.CreateBackup(t.Context(), repo, docbank.BackupOptions{Tag: "docbank:scheduled:reserved"})
	require.ErrorContains(t, err, "reserved")
	points, err := repo.Snapshots()
	require.NoError(t, err)
	require.Empty(t, points)
}

func TestEmbeddedBackupMalformedOwner(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	path := filepath.Join(t.TempDir(), "repo")
	repo, err := docbank.InitBackupRepository(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(path, "docbank-owner"), []byte("damaged"), 0o600))
	_, err = vault.CreateBackup(t.Context(), repo, docbank.BackupOptions{})
	require.ErrorIs(t, err, docbank.ErrBackupRepositoryOwnerInvalid)
	_, err = vault.ClaimBackupRepository(t.Context(), repo, false, "")
	require.ErrorIs(t, err, docbank.ErrBackupRepositoryOwnerInvalid)
}

func TestEmbeddedBackupOwnershipAndClaim(t *testing.T) {
	first, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	second, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	path := filepath.Join(t.TempDir(), "repo")
	repo, err := docbank.InitBackupRepository(path)
	require.NoError(t, err)
	_, err = first.CreateBackup(t.Context(), repo, docbank.BackupOptions{})
	require.NoError(t, err)
	_, err = first.CreateBackup(t.Context(), repo, docbank.BackupOptions{})
	require.NoError(t, err)
	_, err = second.CreateBackup(t.Context(), repo, docbank.BackupOptions{})
	require.ErrorIs(t, err, docbank.ErrBackupRepositoryOwnerConflict)
	_, err = second.ClaimBackupRepository(t.Context(), repo, true, "")
	require.ErrorIs(t, err, docbank.ErrBackupRepositoryOwnerConflict)
	require.NoError(t, os.Remove(filepath.Join(path, "docbank-owner")))
	_, err = first.CreateBackup(t.Context(), repo, docbank.BackupOptions{})
	require.ErrorIs(t, err, docbank.ErrBackupRepositoryClaimRequired)
	preview, err := first.ClaimBackupRepository(t.Context(), repo, false, "")
	require.NoError(t, err)
	require.False(t, preview.Owned)
	require.Equal(t, 2, preview.SnapshotCount)
	points, err := repo.Snapshots()
	require.NoError(t, err)
	_, err = repo.Forget(t.Context(), docbank.BackupForgetOptions{SnapshotIDs: []string{points[0].ID}})
	require.NoError(t, err)
	_, err = first.ClaimBackupRepository(t.Context(), repo, true, preview.Preview)
	require.ErrorIs(t, err, docbank.ErrBackupRepositoryClaimChanged)
	_, err = os.Stat(filepath.Join(path, "docbank-owner"))
	require.ErrorIs(t, err, os.ErrNotExist)
	preview, err = first.ClaimBackupRepository(t.Context(), repo, false, "")
	require.NoError(t, err)
	require.Equal(t, 1, preview.SnapshotCount)
	preview, err = first.ClaimBackupRepository(t.Context(), repo, true, preview.Preview)
	require.NoError(t, err)
	require.True(t, preview.Owned)
	_, err = first.CreateBackup(t.Context(), repo, docbank.BackupOptions{})
	require.NoError(t, err)
	points, err = repo.Snapshots()
	require.NoError(t, err)
	require.Len(t, points, 2)
}
