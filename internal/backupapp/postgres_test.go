package backupapp_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/pgstoretest"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/backup"
)

func postgresArchiveFixture(t *testing.T) *archiveFixture {
	t.Helper()
	setup, cfg, connection := pgstoretest.NewDatabase(t)
	dir := t.TempDir()
	metadata, err := store.OpenPostgres(t.Context(), connection, cfg, dir)
	require.NoError(t, err)
	fixture := archiveFixtureWithStore(t, dir, metadata)
	discard := fixture.discard
	fixture.discard = func() {
		discard()
		_, err := setup.ExecContext(t.Context(), `DROP SCHEMA documents CASCADE`)
		require.NoError(t, err)
	}
	return fixture
}

func TestBackupLooseRestorePostgres(t *testing.T) {
	checkJSONLLooseSnapshotVerifyAndRestore(t, postgresArchiveFixture(t))
}
func TestBackupPackedRestorePostgres(t *testing.T) {
	checkPackedSnapshotRequiresAndUsesPackedRestoreTarget(t, postgresArchiveFixture(t))
}
func TestBackupPhotoSourceLossPostgres(t *testing.T) {
	checkPhotoBackupRoundTrip(t, postgresArchiveFixture(t))
}
func TestBackupAuditedHistoryPostgres(t *testing.T) {
	checkAuditedIncrementalSnapshotRestoresCompleteProtectedHistory(t, postgresArchiveFixture(t))
}
func TestBackupMissingContentPostgres(t *testing.T) {
	fixture := postgresArchiveFixture(t)
	node, err := fixture.metadata.NodeByPath(t.Context(), "/alpha.txt")
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(fixture.root, "blobs", node.BlobHash[:2], node.BlobHash)))
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	manifest, err := backupapp.Create(t.Context(), repo, "test-version", fixture.metadata, fixture.blobs, backup.CreateOptions{})
	require.ErrorContains(t, err, node.BlobHash)
	require.Nil(t, manifest)
	snapshots, err := repo.ListSnapshots()
	require.NoError(t, err)
	require.Empty(t, snapshots)
}
