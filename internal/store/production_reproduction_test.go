package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/production"
)

func TestRetainProductionReproductionReplaysWithoutNewNumbers(t *testing.T) {
	s, finalized, job := unreservedProductionCheckpointFixture(t)
	f := &realRestartFixture{Store: s, root: filepath.Dir(s.path)}
	var err error
	f.blobs, err = openRestartBlobs(s, f.root)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, f.blobs.Close()) })
	written, err := f.write(t.Context(), bytes.NewReader([]byte("synthetic derived PDF")))
	require.NoError(t, err)
	require.Equal(t, finalized.Authority.Prepared.Members[0].Member.PDFSHA256, written.Hash)
	jobRequest := production.JobRequest{JobID: job.ID, OperationID: job.OperationID,
		SetID: job.SetID, Revision: job.Revision, ETag: job.ETag,
		RevisionSHA256: job.RevisionSHA256, PreparedInputSHA256: job.PreparedInputSHA256,
		NumberingProfileSHA256: finalized.Draft.NumberingRecipeSHA256}
	published, err := f.worker().RunJob(t.Context(), jobRequest)
	require.NoError(t, err)
	require.Equal(t, production.ProductionJobSucceeded, published.State)
	originalReceipt, originalManifest := published.Receipt, published.Manifest
	var allocationsBefore int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM bates_allocations`).Scan(&allocationsBefore))
	policy := production.PackageDeliveryPolicy{RecipientCode: "synthetic-recipient", AllowedMethods: []string{"offline-media"}}
	policyRaw, err := canonical.Marshal(policy)
	require.NoError(t, err)
	artifactIDs := make([]string, len(published.Manifest.Artifacts))
	for index, artifact := range published.Manifest.Artifacts {
		artifactIDs[index] = artifact.ID
	}
	request := documentproduction.ReproductionRequest{
		Contract:                        documentproduction.ReproductionRequestContractV1,
		OperationID:                     "88888888-8888-4888-8888-888888888888",
		OriginalProductionReceiptSHA256: originalReceipt.SHA256,
		ArtifactIDs:                     artifactIDs, DeliveryPolicySHA256: productionHash(string(policyRaw)),
	}
	var first, second documentproduction.ReproductionReceipt
	lostResponse := errors.New("synthetic response lost after receipt commit")
	runtime := production.ReproductionRuntime{Catalog: s, Opener: f, StagingDir: t.TempDir(),
		Retain: func(ctx context.Context, got documentproduction.ReproductionRequest,
			_ production.ReproductionSelection, paths production.RecipientPackageRequest,
			_ production.PublishedRecipientPackage) error {
			if first.ID == "" {
				staleQC, readErr := production.ReadPackageQCReceipt(paths.QCPath)
				require.NoError(t, readErr)
				staleQC.ArchiveSHA256 = productionHash("stale archive")
				staleRaw, encodeErr := canonical.Marshal(staleQC)
				require.NoError(t, encodeErr)
				stalePath := filepath.Join(t.TempDir(), "stale-qc.json")
				require.NoError(t, os.WriteFile(stalePath, append(staleRaw, '\n'), 0o600))
				stalePaths := paths
				stalePaths.QCPath = stalePath
				_, staleErr := s.RetainProductionReproduction(ctx, job.ID, got, policy,
					stalePaths, f, restartPackageBlobWriter(f))
				require.Error(t, staleErr)
			}
			receipt, retainErr := s.RetainProductionReproduction(ctx, job.ID, got, policy,
				paths, f, restartPackageBlobWriter(f))
			if first.ID == "" {
				first = receipt
				if retainErr == nil {
					return lostResponse
				}
			} else {
				second = receipt
			}
			return retainErr
		},
	}
	limits := production.PackageLimits{MaxVolumeBytes: 50 << 20, MaxVolumeDocuments: 10}
	_, err = runtime.Run(t.Context(), job.ID, request, policy, "export-dat-opt-images-v1", limits)
	require.ErrorIs(t, err, lostResponse)
	require.NoError(t, documentproduction.ValidateReproductionReceipt(first))
	require.Equal(t, request.OperationID, first.ID)
	require.Equal(t, originalReceipt.SHA256, first.OriginalProductionReceiptSHA256)
	require.Equal(t, originalReceipt.NumberReservationSHA256, first.OriginalNumberReservationSHA256)
	require.Equal(t, originalManifest.SHA256, first.ArtifactManifestSHA256)
	require.Zero(t, first.NumberAllocationCount)
	_, err = runtime.Run(t.Context(), job.ID, request, policy, "export-dat-opt-images-v1", limits)
	require.NoError(t, err)
	require.Equal(t, first, second)
	loaded, err := s.LoadProductionReproduction(t.Context(), job.ID, request.OperationID)
	require.NoError(t, err)
	require.Equal(t, first, loaded)
	retained, err := s.LoadRetainedProductionPackage(t.Context(), job.ID, request.OperationID)
	require.NoError(t, err)
	require.Equal(t, retained.Evidence.QCSHA256, first.PackageQCSHA256)
	var allocationsAfter, operationCount int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM bates_allocations`).Scan(&allocationsAfter))
	require.Equal(t, allocationsBefore, allocationsAfter)
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM production_operation_receipts WHERE operation_id=?`, request.OperationID).Scan(&operationCount))
	require.Equal(t, 1, operationCount)
	require.NoError(t, s.ValidateMetadata(t.Context()))
	after, err := s.LoadProductionJob(t.Context(), job.ID)
	require.NoError(t, err)
	require.Equal(t, originalReceipt, after.Receipt)
	require.Equal(t, originalManifest, after.Manifest)
	changed := runtime
	changed.Retain = func(ctx context.Context, got documentproduction.ReproductionRequest,
		_ production.ReproductionSelection, paths production.RecipientPackageRequest,
		_ production.PublishedRecipientPackage) error {
		_, retainErr := s.RetainProductionReproduction(ctx, job.ID, got, policy,
			paths, f, restartPackageBlobWriter(f))
		return retainErr
	}
	changedLimits := limits
	changedLimits.MaxVolumeBytes++
	_, err = changed.Run(t.Context(), job.ID, request, policy, "export-dat-opt-images-v1", changedLimits)
	require.ErrorIs(t, err, production.ErrReproductionConflict)
	materializeProductionEmailBlobs(t, f)
	driver := productionBackupDriver(t)
	backupRepo := filepath.Join(t.TempDir(), "reproduction-backup")
	require.NoError(t, runProductionBackupDriver(t, driver, "create", f.root, backupRepo))
	restoredRoot := filepath.Join(t.TempDir(), "reproduction-restore")
	require.NoError(t, runProductionBackupDriver(t, driver, "restore", backupRepo, restoredRoot))
	restored, err := Open(filepath.Join(restoredRoot, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ValidateMetadata(t.Context()))
	restoredReceipt, err := restored.LoadProductionReproduction(t.Context(), job.ID, request.OperationID)
	require.NoError(t, err)
	require.Equal(t, first, restoredReceipt)
}
