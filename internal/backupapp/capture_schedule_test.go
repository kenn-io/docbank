package backupapp_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/kit/backup"
)

func TestCaptureReservesScheduledTagAndChecksOwner(t *testing.T) {
	fixture := newArchiveFixture(t)
	path := filepath.Join(t.TempDir(), "repo")
	repo, err := backup.Init(path)
	require.NoError(t, err)
	_, err = backupapp.Create(t.Context(), repo, "test", fixture.metadata, fixture.blobs, backup.CreateOptions{Tag: "docbank:scheduled:"})
	require.ErrorIs(t, err, backupapp.ErrReservedScheduledTag)
	_, err = os.Stat(filepath.Join(path, "docbank-owner"))
	require.ErrorIs(t, err, os.ErrNotExist)
	manual, err := backupapp.Create(t.Context(), repo, "test", fixture.metadata, fixture.blobs, backup.CreateOptions{Tag: "docbank:scheduled"})
	require.NoError(t, err)
	require.Equal(t, "docbank:scheduled", manual.Options.Tag)
	owner, err := os.ReadFile(repo.Path("docbank-owner"))
	require.NoError(t, err)
	require.Equal(t, fixture.metadata.VaultID()+"\n", string(owner))
	point, err := backupapp.CreateScheduled(t.Context(), repo, "test", fixture.metadata, fixture.blobs,
		backup.CreateOptions{Tag: "manual"})
	require.NoError(t, err)
	require.Equal(t, "docbank:scheduled:"+fixture.metadata.VaultID(), point.Options.Tag)
	other := newArchiveFixture(t)
	_, err = backupapp.CreateScheduled(t.Context(), repo, "test", other.metadata, other.blobs, backup.CreateOptions{})
	require.ErrorIs(t, err, backupapp.ErrRepositoryOwnerConflict)
	points, err := repo.ListSnapshots()
	require.NoError(t, err)
	require.Len(t, points, 2)
	require.Equal(t, point.SnapshotID, points[1].SnapshotID)
	require.NoError(t, os.Remove(repo.Path("docbank-owner")))
	_, err = backupapp.Create(t.Context(), repo, "test", fixture.metadata, fixture.blobs, backup.CreateOptions{})
	require.ErrorIs(t, err, backupapp.ErrRepositoryClaimRequired)
	points, err = repo.ListSnapshots()
	require.NoError(t, err)
	require.Len(t, points, 2)
}
