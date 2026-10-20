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
	for _, tag := range []string{"docbank:scheduled:", "docbank:scheduled:" + fixture.metadata.VaultID(), "docbank:scheduled:another-vault"} {
		_, err = backupapp.Create(t.Context(), repo, "test", fixture.metadata, fixture.blobs, backup.CreateOptions{Tag: tag})
		require.ErrorIs(t, err, backupapp.ErrReservedScheduledTag)
	}
	_, err = os.Stat(filepath.Join(path, "docbank-owner"))
	require.ErrorIs(t, err, os.ErrNotExist)
	manual, err := backupapp.Create(t.Context(), repo, "test", fixture.metadata, fixture.blobs, backup.CreateOptions{Tag: "docbank:scheduled"})
	require.NoError(t, err)
	require.Equal(t, "docbank:scheduled", manual.Options.Tag)
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
