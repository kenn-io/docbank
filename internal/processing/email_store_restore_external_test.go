package processing_test

import (
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"

	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/maintenance"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestEmailStoreArchivePhysicalPortabilityAndMaintenance(t *testing.T) {
	t.Parallel()
	f := processing.NewEmailStoreRestoreTestFixture(t)
	buildID := f.PublishBody()
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repository"))
	require.NoError(t, err)
	manifest, err := backupapp.Create(t.Context(), repo, "test", f.Catalog, f.Blobs, backup.CreateOptions{Jobs: 2})
	require.NoError(t, err)
	require.NotEmpty(t, manifest.SnapshotID)
	verified, err := backup.Verify(t.Context(), repo, backupapp.New("test"), backup.VerifyOptions{Jobs: 2})
	require.NoError(t, err)
	require.Empty(t, verified.Problems)
	target := filepath.Join(t.TempDir(), "restored")
	_, err = backupapp.Restore(t.Context(), repo, "test", backup.RestoreOptions{TargetDir: target, Jobs: 2})
	require.NoError(t, err)
	restored, err := store.OpenForRestore(filepath.Join(target, "docbank.db"), store.DefaultSQLiteDriver())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	bs, err := blob.New(store.NewPackCatalog(restored), filepath.Join(target, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bs.Close()) })
	got, err := restored.EmailMetadata(t.Context(), f.Email.Version.ID)
	require.NoError(t, err)
	require.Equal(t, "available", got.BodySearch.State)
	require.Equal(t, buildID, *got.BodySearch.RenditionBuildID)
	for _, a := range f.Email.Generation.Artifacts {
		receipt, err := restored.EmailPart(t.Context(), f.Email.Version.ID, f.Email.Generation.ID, a.PartPath, a.Role)
		require.NoError(t, err)
		r, size, err := bs.OpenStreamContext(t.Context(), receipt.BlobSHA256)
		require.NoError(t, err)
		require.Equal(t, a.Size, size)
		b, err := io.ReadAll(r)
		require.NoError(t, err)
		require.NoError(t, r.Close())
		require.Equal(t, f.Payloads[a.BlobSHA256], b)
	}
	require.NoError(t, restored.VerifyRenditionBlobBytes(t.Context(), bs))
	require.NoError(t, restored.ValidateMetadata(t.Context()))
	// One independently retained file sharing an email payload must survive purge.
	body := f.Email.Evidence.Inventory.Parts[0].BodyUTF8
	_, err = f.Catalog.CreateFile(t.Context(), f.Catalog.RootID(), "independent-body.txt", body.SHA256, body.Size, "text/plain")
	require.NoError(t, err)
	report, err := maintenance.PurgeDerivatives(t.Context(), f.Catalog, f.Blobs, store.PurgeRequest{All: true})
	require.NoError(t, err)
	require.Equal(t, 1, report.Purge.RemovedEmailGenerations)
	require.Equal(t, len(f.Email.Generation.Artifacts), report.Purge.RemovedEmailPartArtifacts)
	for hash := range f.Payloads {
		exists, err := f.Blobs.Exists(hash)
		require.NoError(t, err)
		if hash == body.SHA256 {
			require.True(t, exists)
		} else {
			require.False(t, exists)
		}
	}
	_, err = restored.EmailMetadata(t.Context(), f.Email.Version.ID)
	require.NoError(t, err, "immutable backup restore is independent of later source purge")
	verified, err = backup.Verify(t.Context(), repo, backupapp.New("test"), backup.VerifyOptions{Jobs: 2})
	require.NoError(t, err)
	require.Empty(t, verified.Problems)
}

func TestEmailStoreMissingRetainedPartBytesFailVerification(t *testing.T) {
	t.Parallel()
	f := processing.NewEmailStoreRestoreTestFixture(t)
	require.NoError(t, f.Catalog.VerifyRenditionBlobBytes(t.Context(), f.Blobs))
	header := f.Email.Evidence.Inventory.Parts[0].HeaderBlock
	require.NoError(t, f.Blobs.Remove(header.SHA256))
	require.Error(t, f.Catalog.VerifyRenditionBlobBytes(t.Context(), f.Blobs))
	repo, err := backup.Init(filepath.Join(t.TempDir(), "incomplete-repository"))
	require.NoError(t, err)
	_, err = backupapp.Create(t.Context(), repo, "test", f.Catalog, f.Blobs, backup.CreateOptions{Jobs: 2})
	require.Error(t, err, "portable capture must require the retained raw header blob")
}
