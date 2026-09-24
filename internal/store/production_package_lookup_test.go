package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/production"
)

func TestLoadRetainedProductionPackageReopensExactEvidence(t *testing.T) {
	f, job := publishedRealRetentionFixture(t)
	dir := t.TempDir()
	request := production.RecipientPackageRequest{
		JobID: job.ID, ProfileID: "export-dat-opt-images-v1",
		Limits:      production.PackageLimits{MaxVolumeBytes: 50 << 20, MaxVolumeDocuments: 10},
		ArchivePath: filepath.Join(dir, "recipient.zip"), QCPath: filepath.Join(dir, "qc.json"),
		TransmittalPath: filepath.Join(dir, "transmittal.json"),
	}
	_, err := production.PublishRecipientPackage(t.Context(), f.Store, f, request)
	require.NoError(t, err)
	const operationID = "77777777-7777-4777-8777-777777777777"
	written, err := f.RetainProductionPackage(t.Context(), job.ID, operationID,
		request.ProfileID, request.Limits, request.ArchivePath, request.QCPath,
		request.TransmittalPath, restartPackageBlobWriter(f))
	require.NoError(t, err)
	f.reopen(t)
	loaded, err := f.LoadRetainedProductionPackage(t.Context(), job.ID, operationID)
	require.NoError(t, err)
	require.Equal(t, written.Evidence, loaded.Evidence)
	for _, pair := range [][2]ContentWriteReceipt{
		{written.Archive, loaded.Archive}, {written.QC, loaded.QC},
		{written.Transmittal, loaded.Transmittal},
	} {
		require.Equal(t, pair[0].Node.ID, pair[1].Node.ID)
		require.Equal(t, pair[0].Version.ID, pair[1].Version.ID)
		require.Equal(t, pair[0].Version.BlobHash, pair[1].Version.BlobHash)
		require.Equal(t, pair[0].Physical, pair[1].Physical)
	}
	_, err = f.LoadRetainedProductionPackage(t.Context(), job.ID,
		"88888888-8888-4888-8888-888888888888")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = f.db.ExecContext(t.Context(), `DELETE FROM provenance_version_bindings
		WHERE content_version_id=?`, loaded.QC.Version.ID)
	require.NoError(t, err)
	_, err = f.LoadRetainedProductionPackage(t.Context(), job.ID, operationID)
	require.ErrorIs(t, err, production.ErrPackageEvidence)
}
