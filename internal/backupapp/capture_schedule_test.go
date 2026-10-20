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
