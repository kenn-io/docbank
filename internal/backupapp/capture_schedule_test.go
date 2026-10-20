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
	_, err = backupapp.Create(t.Context(), repo, "test", fixture.metadata, fixture.blobs,
		backup.CreateOptions{Tag: backupapp.ScheduledTag})
	require.ErrorIs(t, err, backupapp.ErrReservedScheduledTag)
	_, err = os.Stat(filepath.Join(path, "docbank-owner"))
	require.ErrorIs(t, err, os.ErrNotExist)
	point, err := backupapp.CreateScheduled(t.Context(), repo, "test", fixture.metadata, fixture.blobs,
		backup.CreateOptions{Tag: "manual"})
	require.NoError(t, err)
	require.Equal(t, "docbank:scheduled", point.Options.Tag)
	other := newArchiveFixture(t)
	_, err = backupapp.CreateScheduled(t.Context(), repo, "test", other.metadata, other.blobs, backup.CreateOptions{})
	require.ErrorIs(t, err, backupapp.ErrRepositoryOwnerConflict)
	points, err := repo.ListSnapshots()
	require.NoError(t, err)
	require.Len(t, points, 1)
	require.Equal(t, point.SnapshotID, points[0].SnapshotID)
}

func TestRepositoryOwnerCheckDuringSharedRead(t *testing.T) {
	fixture := newArchiveFixture(t)
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	require.NoError(t, backupapp.EnsureRepositoryOwner(t.Context(), repo, fixture.metadata.VaultID(), false))
	reader, err := repo.AcquireSharedLock("test-verify-reader", false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Release()) })
	require.NoError(t, backupapp.EnsureRepositoryOwner(t.Context(), repo, fixture.metadata.VaultID(), false))
	require.ErrorIs(t, backupapp.EnsureRepositoryOwner(t.Context(), repo, "another-vault", false), backupapp.ErrRepositoryOwnerConflict)
}
