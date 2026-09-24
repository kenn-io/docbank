package store

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/production"
	"go.kenn.io/kit/packstore"
)

func restartPackageBlobWriter(f *realRestartFixture) ProductionPackageBlobWriter {
	return func(ctx context.Context, reader io.Reader) (string, int64, BlobPhysical, error) {
		written, err := f.blobs.loose.Write(ctx, reader, packstore.WriteOptions{
			Durability: packstore.DurablePublication, Dedup: packstore.VerifyTypeAndSize,
			MaxBytes: 100 << 20,
		})
		if err != nil {
			return "", 0, BlobPhysical{}, fmt.Errorf("writing package blob: %w", err)
		}
		return written.Hash.String(), written.Size,
			BlobPhysical{Encoding: "raw", StoredBytes: written.StoredSize, Created: written.Created}, nil
	}
}

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
	const packageOperationID = "77777777-7777-4777-8777-777777777777"
	writePackageBlob := restartPackageBlobWriter(f)
	retained, err := s.RetainProductionPackage(t.Context(), job.ID, packageOperationID,
		packageRequest.ProfileID, packageRequest.Limits, archive, packageRequest.QCPath,
		packageRequest.TransmittalPath, writePackageBlob)
	require.NoError(t, err)
	require.Equal(t, first.QC.ArchiveSHA256, retained.Archive.Version.BlobHash)
	require.Equal(t, retained.Evidence.ArchiveSHA256, retained.Archive.Version.BlobHash)
	var packageProvenance string
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT i.source_desc FROM ingests i
		JOIN provenance p ON p.ingest_id=i.id WHERE p.node_id=? AND i.source_kind='embedded:production-package'`,
		retained.Archive.Node.ID).Scan(&packageProvenance))
	require.Contains(t, packageProvenance, retained.Evidence.SHA256)
	require.NotContains(t, packageProvenance, "source_version_id")
	for _, name := range []string{"recipient.zip", "qc.json", "transmittal.json"} {
		_, err := s.NodeByPath(t.Context(), "/productions/"+job.ID+"/packages/"+packageOperationID+"/"+name)
		require.NoError(t, err)
	}
	retainedAgain, err := s.RetainProductionPackage(t.Context(), job.ID, packageOperationID,
		packageRequest.ProfileID, packageRequest.Limits, archive, packageRequest.QCPath,
		packageRequest.TransmittalPath, writePackageBlob)
	require.NoError(t, err)
	require.Equal(t, retained.Archive.Version.ID, retainedAgain.Archive.Version.ID)
	require.Equal(t, retained.QC.Version.ID, retainedAgain.QC.Version.ID)
	require.Equal(t, retained.Transmittal.Version.ID, retainedAgain.Transmittal.Version.ID)
	require.Equal(t, retained.Archive.Physical, retainedAgain.Archive.Physical)
	require.Equal(t, retained.QC.Physical, retainedAgain.QC.Physical)
	require.Equal(t, retained.Transmittal.Physical, retainedAgain.Transmittal.Physical)
	unreachable, err := s.UnreachableBlobs(t.Context())
	require.NoError(t, err)
	for _, part := range []ContentWriteReceipt{retained.Archive, retained.QC, retained.Transmittal} {
		require.NotContains(t, blobHashes(unreachable), part.Version.BlobHash)
	}
	materializeProductionEmailBlobs(t, f)
	driver := productionBackupDriver(t)
	backupRepo := filepath.Join(t.TempDir(), "package-backup")
	require.NoError(t, runProductionBackupDriver(t, driver, "create", f.root, backupRepo))
	restoredRoot := filepath.Join(t.TempDir(), "package-restore")
	require.NoError(t, runProductionBackupDriver(t, driver, "restore", backupRepo, restoredRoot))
	restoredStore, err := Open(filepath.Join(restoredRoot, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restoredStore.Close()) })
	restoredBlobs, err := openRestartBlobs(restoredStore, restoredRoot)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restoredBlobs.Close()) })
	for name, path := range map[string]string{
		"recipient.zip":    archive,
		"qc.json":          packageRequest.QCPath,
		"transmittal.json": packageRequest.TransmittalPath,
	} {
		node, readErr := restoredStore.NodeByPath(t.Context(),
			"/productions/"+job.ID+"/packages/"+packageOperationID+"/"+name)
		require.NoError(t, readErr)
		want, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		stream, size, readErr := restoredBlobs.OpenStreamContext(t.Context(), node.BlobHash)
		require.NoError(t, readErr)
		got, readErr := io.ReadAll(stream)
		require.NoError(t, readErr)
		require.NoError(t, stream.Verify())
		require.NoError(t, stream.Close())
		require.Equal(t, int64(len(want)), size)
		require.Equal(t, want, got)
	}
	for _, sidecar := range []string{packageRequest.QCPath, packageRequest.TransmittalPath} {
		original, readErr := os.ReadFile(sidecar)
		require.NoError(t, readErr)
		require.NoError(t, os.Chmod(sidecar, 0o600))
		require.NoError(t, os.WriteFile(sidecar, append(bytes.Clone(original), '\n'), 0o600))
		_, readErr = s.RetainProductionPackage(t.Context(), job.ID, packageOperationID,
			packageRequest.ProfileID, packageRequest.Limits, archive, packageRequest.QCPath,
			packageRequest.TransmittalPath, writePackageBlob)
		require.ErrorIs(t, readErr, production.ErrPackageEvidence)
		require.NoError(t, os.WriteFile(sidecar, original, 0o600))
		require.NoError(t, os.Chmod(sidecar, 0o400))
	}
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
