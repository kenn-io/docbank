package store

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/production"
)

func TestPublishedProductionPackageUsesStoredArtifactsAfterSourceHeadChange(t *testing.T) {
	s, finalized, job := unreservedProductionCheckpointFixture(t)
	f := &realRestartFixture{Store: s, root: filepath.Dir(s.path)}
	var err error
	f.blobs, err = openRestartBlobs(s, f.root)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, f.blobs.Close()) })
	written, err := f.write(t.Context(), bytes.NewReader([]byte("synthetic derived PDF")))
	require.NoError(t, err)
	require.Equal(t, finalized.Authority.Prepared.Members[0].Member.PDFSHA256, written.Hash)
	request := production.JobRequest{JobID: job.ID, OperationID: job.OperationID, SetID: job.SetID,
		Revision: job.Revision, ETag: job.ETag, RevisionSHA256: job.RevisionSHA256,
		PreparedInputSHA256:    job.PreparedInputSHA256,
		NumberingProfileSHA256: finalized.Draft.NumberingRecipeSHA256}
	published, err := f.worker().RunJob(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, production.ProductionJobSucceeded, published.State)
	member := finalized.Authority.Prepared.Members[0].Member
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		const newVersion = "76000000-0000-4000-8000-000000000004"
		_, err := tx.ExecContext(t.Context(), `INSERT INTO content_versions(version_id,node_id,blob_hash,size,recorded_at,node_revision,introduced_operation_id,transition_kind)
			SELECT ?,node_id,blob_hash,size,?,node_revision+1,?,'content_replace' FROM content_versions WHERE version_id=?`,
			newVersion, nowRFC3339(), "76000000-0000-4000-8000-000000000005", member.SourceVersionID)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(t.Context(), `UPDATE nodes SET current_version_id=?,revision=revision+1 WHERE id=?`, newVersion, member.NodeID)
		return err
	}))
	_, err = s.LoadFinalizedProduction(t.Context(), job.SetID, job.Revision)
	require.ErrorIs(t, err, ErrInvalidProduction)

	archive := filepath.Join(t.TempDir(), "recipient.zip")
	qcDir := filepath.Join(filepath.Dir(archive), "qc")
	packageRequest := production.RecipientPackageRequest{JobID: job.ID, ProfileID: "export-dat-opt-images-v1",
		Limits:      production.PackageLimits{MaxVolumeBytes: 50 << 20, MaxVolumeDocuments: 10},
		ArchivePath: archive, QCPath: filepath.Join(qcDir, "receipt.json"), TransmittalPath: archive + ".transmittal.json"}
	_, err = production.PublishRecipientPackage(t.Context(), f.Store, f, packageRequest)
	require.Error(t, err, "a sidecar failure must leave the verified archive available for retry")
	sealed, err := os.ReadFile(archive)
	require.NoError(t, err)
	_, statErr := os.Stat(packageRequest.TransmittalPath)
	require.ErrorIs(t, statErr, os.ErrNotExist)
	require.NoError(t, os.Mkdir(qcDir, 0o700))
	first, err := production.PublishRecipientPackage(t.Context(), f.Store, f, packageRequest)
	require.NoError(t, err)
	afterRetry, err := os.ReadFile(archive)
	require.NoError(t, err)
	require.Equal(t, sealed, afterRetry, "retry must not rewrite the final archive")
	require.Len(t, first.Manifest.Documents, 2)
	require.NoError(t, production.VerifyRecipientArchiveWithQC(archive, first.QC))
	_, err = production.ReadPackageQCReceipt(packageRequest.QCPath)
	require.NoError(t, err)
	_, err = production.ReadRecipientTransmittal(packageRequest.TransmittalPath)
	require.NoError(t, err)

	replayed, err := production.PublishRecipientPackage(t.Context(), f.Store, f, packageRequest)
	require.NoError(t, err)
	require.Equal(t, first, replayed)
	var page documentproduction.Artifact
	for _, artifact := range published.Manifest.Artifacts {
		if artifact.Role == documentproduction.ArtifactRoleRedactedPage {
			page = artifact
			break
		}
	}
	require.NotEmpty(t, page.ID)
	pagePath := filepath.Join(f.root, "blobs", page.SHA256[:2], page.SHA256)
	require.NoError(t, os.Rename(pagePath, pagePath+".missing"))
	missing := packageRequest
	missing.ArchivePath = filepath.Join(t.TempDir(), "missing.zip")
	missing.QCPath, missing.TransmittalPath = missing.ArchivePath+".qc.json", missing.ArchivePath+".transmittal.json"
	_, err = production.PublishRecipientPackage(t.Context(), f.Store, f, missing)
	require.Error(t, err)
	_, statErr = os.Stat(missing.ArchivePath)
	require.ErrorIs(t, statErr, os.ErrNotExist)
	_, statErr = os.Stat(missing.QCPath)
	require.ErrorIs(t, statErr, os.ErrNotExist)
	require.NoError(t, os.Rename(pagePath+".missing", pagePath))

	contents, err := os.ReadFile(archive)
	require.NoError(t, err)
	contents[len(contents)-1] ^= 0xff
	require.NoError(t, os.Chmod(archive, 0o600))
	require.NoError(t, os.WriteFile(archive, contents, 0o600))
	_, err = production.PublishRecipientPackage(t.Context(), f.Store, f, packageRequest)
	require.ErrorIs(t, err, production.ErrRecipientArchive)
}
