package backupapp_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
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
	claim, err := backupapp.PreviewRepositoryClaim(t.Context(), repo, fixture.metadata)
	require.NoError(t, err)
	require.True(t, claim.Owned)
	// Ownership changes no Kit format, so releases without it still open the repository.
	reopened, err := backup.Open(path)
	require.NoError(t, err)
	require.Equal(t, 1, reopened.Config().MinReaderVersion)
	point, err := backupapp.CreateScheduled(t.Context(), repo, "test", fixture.metadata, fixture.blobs,
		backup.CreateOptions{Tag: "manual"})
	require.NoError(t, err)
	require.Equal(t, "docbank:scheduled:"+fixture.metadata.VaultID(), point.Options.Tag)
	other := newArchiveFixture(t)
	_, err = backupapp.CreateScheduled(t.Context(), repo, "test", other.metadata, other.blobs, backup.CreateOptions{})
	require.ErrorIs(t, err, backupapp.ErrRepositoryOwnerConflict)
	_, err = backupapp.PreviewRepositoryClaim(t.Context(), repo, other.metadata)
	require.ErrorIs(t, err, backupapp.ErrRepositoryOwnerConflict)
	points, err := repo.ListSnapshots()
	require.NoError(t, err)
	require.Len(t, points, 2)
	require.Equal(t, point.SnapshotID, points[1].SnapshotID)
}

func TestUnmarkedHistoryRequiresReviewedClaim(t *testing.T) {
	fixture := newArchiveFixture(t)
	// A release without ownership leaves history with no marker.
	path := filepath.Join(t.TempDir(), "claimed")
	repo, err := backup.Init(path)
	require.NoError(t, err)
	olderCapture := func() {
		t.Helper()
		_, err := backup.Create(t.Context(), repo, backupapp.New("test"), backup.CreateOptions{
			MetadataSource: backupapp.NewMetadataSource(fixture.metadata),
			ContentSource:  backupapp.NewContentSource(fixture.blobs),
			SQLiteOpener:   backupapp.SQLiteOpener(fixture.metadata.SQLiteDriver()),
		})
		require.NoError(t, err)
	}
	olderCapture()
	_, err = backupapp.Create(t.Context(), repo, "test", fixture.metadata, fixture.blobs, backup.CreateOptions{})
	require.ErrorIs(t, err, backupapp.ErrRepositoryClaimRequired)
	preview, err := backupapp.PreviewRepositoryClaim(t.Context(), repo, fixture.metadata)
	require.NoError(t, err)
	require.False(t, preview.Owned)
	require.False(t, preview.Takeover)
	require.Equal(t, 1, preview.SnapshotCount)

	// History added after the review is not assigned by that review.
	olderCapture()
	_, err = backupapp.ClaimRepository(t.Context(), repo, fixture.metadata, preview.Fingerprint)
	require.ErrorIs(t, err, backupapp.ErrRepositoryClaimChanged)
	_, err = os.Stat(repo.Path("docbank-owner"))
	require.ErrorIs(t, err, os.ErrNotExist)

	preview, err = backupapp.PreviewRepositoryClaim(t.Context(), repo, fixture.metadata)
	require.NoError(t, err)
	require.Equal(t, 2, preview.SnapshotCount)
	claimed, err := backupapp.ClaimRepository(t.Context(), repo, fixture.metadata, preview.Fingerprint)
	require.NoError(t, err)
	require.True(t, claimed.Owned)
	reopened, err := backup.Open(path)
	require.NoError(t, err)
	require.Equal(t, 1, reopened.Config().MinReaderVersion)
	_, err = backupapp.Create(t.Context(), repo, "test", fixture.metadata, fixture.blobs, backup.CreateOptions{})
	require.NoError(t, err)
	points, err := repo.ListSnapshots()
	require.NoError(t, err)
	require.Len(t, points, 3)
}

func TestRestoredVaultMustClaimRepositoryBeforeCapture(t *testing.T) {
	fixture := newArchiveFixture(t)
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	manifest, err := backupapp.Create(t.Context(), repo, "test", fixture.metadata, fixture.blobs, backup.CreateOptions{})
	require.NoError(t, err)
	restoredStore, restoredBlobs := restoredCopy(t, repo)
	require.Equal(t, fixture.metadata.VaultID(), restoredStore.VaultID())

	repositoryFiles := func() map[string]string {
		files := map[string]string{}
		require.NoError(t, filepath.WalkDir(repo.Root(), func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			content, err := os.ReadFile(path)
			files[path] = string(content)
			return err
		}))
		return files
	}
	before := repositoryFiles()
	_, err = backupapp.Create(t.Context(), repo, "test", restoredStore, restoredBlobs, backup.CreateOptions{})
	require.ErrorIs(t, err, backupapp.ErrRepositoryClaimRequired)
	require.Equal(t, before, repositoryFiles())
	preview, err := backupapp.PreviewRepositoryClaim(t.Context(), repo, restoredStore)
	require.NoError(t, err)
	require.False(t, preview.Owned)
	require.True(t, preview.Takeover)
	require.Equal(t, manifest.CreatedAt, preview.NewestAt)
	_, err = backupapp.ClaimRepository(t.Context(), repo, restoredStore, preview.Fingerprint)
	require.NoError(t, err)
	_, err = backupapp.Create(t.Context(), repo, "test", restoredStore, restoredBlobs, backup.CreateOptions{})
	require.NoError(t, err)

	// The original installation no longer captures into the claimed repository.
	before = repositoryFiles()
	_, err = backupapp.Create(t.Context(), repo, "test", fixture.metadata, fixture.blobs, backup.CreateOptions{})
	require.ErrorIs(t, err, backupapp.ErrRepositoryClaimRequired)
	require.Equal(t, before, repositoryFiles())
}

// restoredCopy restores repo's newest snapshot as a new installation of the
// same vault, the way an operator recovers onto another machine.
func restoredCopy(t *testing.T, repo *backup.Repo) (*store.Store, *blob.Store) {
	t.Helper()
	target := filepath.Join(t.TempDir(), "restored")
	_, err := backupapp.Restore(t.Context(), repo, "test", backup.RestoreOptions{TargetDir: target})
	require.NoError(t, err)
	restored, err := store.OpenForRestore(filepath.Join(target, "docbank.db"), store.DefaultSQLiteDriver())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(restored), filepath.Join(target, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	return restored, blobs
}

// A claim that lands after a capture starts must still stop that capture.
// Not parallel: it replaces the package capture hook.
func TestCaptureRefusesTakeoverBeforeRepositoryLock(t *testing.T) {
	fixture := newArchiveFixture(t)
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	_, err = backupapp.Create(t.Context(), repo, "test", fixture.metadata, fixture.blobs, backup.CreateOptions{})
	require.NoError(t, err)
	restored, _ := restoredCopy(t, repo)
	claimed := false
	backupapp.SetBeforeCaptureLock(t, func() {
		if claimed {
			return
		}
		claimed = true
		preview, err := backupapp.PreviewRepositoryClaim(t.Context(), repo, restored)
		require.NoError(t, err)
		_, err = backupapp.ClaimRepository(t.Context(), repo, restored, preview.Fingerprint)
		require.NoError(t, err)
	})
	_, err = backupapp.Create(t.Context(), repo, "test", fixture.metadata, fixture.blobs, backup.CreateOptions{})
	require.ErrorIs(t, err, backupapp.ErrRepositoryClaimRequired)
	require.True(t, claimed)
	points, err := repo.ListSnapshots()
	require.NoError(t, err)
	require.Len(t, points, 1)
}
