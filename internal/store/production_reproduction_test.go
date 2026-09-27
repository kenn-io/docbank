package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/production"
)

func TestReproductionReceiptTimestampIsCanonicalWithTrailingZero(t *testing.T) {
	when := time.Date(2026, time.September, 27, 5, 30, 0, 123456780, time.UTC)
	createdAt := reproductionReceiptTimestamp(when)
	require.Equal(t, "2026-09-27T05:30:00.12345678Z", createdAt)
	receipt := documentproduction.ReproductionReceipt{
		Contract:                        documentproduction.ReproductionReceiptContractV1,
		ID:                              "88888888-8888-4888-8888-888888888888",
		OriginalProductionReceiptSHA256: productionHash("original"),
		OriginalNumberReservationSHA256: productionHash("numbers"),
		ArtifactManifestSHA256:          productionHash("manifest"),
		PackageQCSHA256:                 productionHash("qc"),
		DeliveryPolicySHA256:            productionHash("policy"),
		CreatedAt:                       createdAt,
	}
	_, _, err := documentproduction.CanonicalReproductionReceipt(receipt)
	require.NoError(t, err)
}

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
	originalSource := finalized.Authority.Prepared.Members[0].Member
	replacement, err := f.write(t.Context(), bytes.NewReader([]byte("synthetic replacement source PDF")))
	require.NoError(t, err)
	liveSource, err := s.NodeByID(t.Context(), originalSource.NodeID)
	require.NoError(t, err)
	_, newerVersion, err := s.ReplaceContent(t.Context(), liveSource.ID, liveSource.Revision,
		replacement.Hash, replacement.Size, liveSource.MimeType)
	require.NoError(t, err)
	require.NotEqual(t, originalSource.SourceVersionID, newerVersion.ID)
	_, err = s.LoadFinalizedProduction(t.Context(), job.SetID, job.Revision)
	require.ErrorIs(t, err, ErrInvalidProduction)
	var allocationsBefore int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM bates_allocations`).Scan(&allocationsBefore))
	policy := production.PackageDeliveryPolicy{RecipientCode: "synthetic-recipient", AllowedMethods: []string{"offline-media"}}
	policyRaw, err := canonical.Marshal(policy)
	require.NoError(t, err)
	artifactIDs := make([]string, len(published.Manifest.Artifacts))
	for index, artifact := range published.Manifest.Artifacts {
		artifactIDs[index] = artifact.ID
	}
	packageInputs, err := s.LoadProductionPackageInputs(t.Context(), job.ID)
	require.NoError(t, err)
	sourceVersionIDs := make([]string, len(packageInputs.Members))
	for index, member := range packageInputs.Members {
		sourceVersionIDs[index] = member.SourceVersionID
	}
	require.Equal(t, originalSource.SourceVersionID, sourceVersionIDs[0])
	label := packageInputs.Reservation.Numbers[0].Text
	originalNumber, err := s.FindPublishedProductionNumber(t.Context(), label)
	require.NoError(t, err)
	require.Equal(t, originalSource.SourceVersionID, originalNumber.SourceVersionID)
	require.Equal(t, originalReceipt.SHA256, originalNumber.ProductionReceiptSHA256)
	require.Equal(t, originalManifest.SHA256, originalNumber.ArtifactManifestSHA256)
	request := documentproduction.ReproductionRequest{
		Contract:                        documentproduction.ReproductionRequestContractV1,
		OperationID:                     "88888888-8888-4888-8888-888888888888",
		OriginalProductionReceiptSHA256: originalReceipt.SHA256,
		ArtifactIDs:                     artifactIDs, SourceVersionIDs: sourceVersionIDs,
		DeliveryPolicySHA256: productionHash(string(policyRaw)),
	}
	_, err = production.PrepareReproduction(t.Context(), s, f, job.ID, request, policy)
	require.NoError(t, err)
	var first, second documentproduction.ReproductionReceipt
	const deliveryOperationID = "99999999-9999-4999-8999-999999999999"
	proofPath := filepath.Join(t.TempDir(), "synthetic-transfer-proof.txt")
	require.NoError(t, os.WriteFile(proofPath, []byte("synthetic transfer acknowledged"), 0o600))
	evidence := production.PackageDeliveryEvidence{RecipientCode: policy.RecipientCode,
		Method: "offline-media", DeliveredAt: "2026-09-23T21:00:00Z", ProofPath: proofPath}
	var firstDelivery, secondDelivery ReproductionDeliveryRecord
	interruptedBeforeReceipt := errors.New("synthetic interruption after package retention")
	lostResponse := errors.New("synthetic response lost after receipt commit")
	interrupted := false
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
			if !interrupted {
				_, partialErr := s.RetainProductionPackage(ctx, job.ID, got.OperationID,
					paths.ProfileID, paths.Limits, paths.ArchivePath, paths.QCPath,
					paths.TransmittalPath, restartPackageBlobWriter(f))
				require.NoError(t, partialErr)
				interrupted = true
				return interruptedBeforeReceipt
			}
			receipt, retainErr := s.RetainProductionReproduction(ctx, job.ID, got, policy,
				paths, f, restartPackageBlobWriter(f))
			if retainErr != nil {
				return fmt.Errorf("retain reproduction: %w", retainErr)
			}
			if first.ID == "" {
				wrongPolicy := production.PackageDeliveryPolicy{RecipientCode: "wrong-recipient",
					AllowedMethods: []string{"offline-media"}}
				wrongEvidence := evidence
				wrongEvidence.RecipientCode = wrongPolicy.RecipientCode
				wrongPath := filepath.Join(filepath.Dir(paths.ArchivePath), "wrong-delivery.json")
				_, wrongErr := s.RecordProductionReproductionDelivery(ctx, job.ID,
					got.OperationID, deliveryOperationID, wrongPolicy, wrongEvidence, paths,
					wrongPath, restartPackageBlobWriter(f))
				require.Error(t, wrongErr)
				_, statErr := os.Stat(wrongPath)
				require.ErrorIs(t, statErr, os.ErrNotExist)
				alternateDir := t.TempDir()
				alternatePaths := production.RecipientPackageRequest{
					JobID: job.ID, ProfileID: "export-dat-pdf-v1", Limits: paths.Limits,
					ArchivePath:     filepath.Join(alternateDir, "recipient.zip"),
					QCPath:          filepath.Join(alternateDir, "qc.json"),
					TransmittalPath: filepath.Join(alternateDir, "transmittal.json"),
				}
				_, alternateErr := production.PublishRecipientPackage(ctx, s, f, alternatePaths)
				require.NoError(t, alternateErr)
				alternateDeliveryPath := filepath.Join(alternateDir, "delivery.json")
				_, alternateErr = s.RecordProductionReproductionDelivery(ctx, job.ID,
					got.OperationID, deliveryOperationID, policy, evidence, alternatePaths,
					alternateDeliveryPath, restartPackageBlobWriter(f))
				require.Error(t, alternateErr)
				_, statErr = os.Stat(alternateDeliveryPath)
				require.ErrorIs(t, statErr, os.ErrNotExist)
			}
			deliveryPath := filepath.Join(filepath.Dir(paths.ArchivePath), "delivery.json")
			delivery, deliveryErr := s.RecordProductionReproductionDelivery(ctx, job.ID,
				got.OperationID, deliveryOperationID, policy, evidence, paths,
				deliveryPath, restartPackageBlobWriter(f))
			if deliveryErr != nil {
				return fmt.Errorf("record reproduction delivery: %w", deliveryErr)
			}
			if first.ID == "" {
				first = receipt
				firstDelivery = delivery
				return lostResponse
			}
			second = receipt
			secondDelivery = delivery
			return nil
		},
	}
	limits := production.PackageLimits{MaxVolumeBytes: 50 << 20, MaxVolumeDocuments: 10}
	changedSource := request
	changedSource.SourceVersionIDs = append([]string(nil), request.SourceVersionIDs...)
	changedSource.SourceVersionIDs[0] = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	_, err = runtime.Run(t.Context(), job.ID, changedSource, policy, "export-dat-opt-images-v1", limits)
	require.ErrorIs(t, err, production.ErrReproductionConflict)
	_, err = runtime.Run(t.Context(), job.ID, request, policy, "export-dat-opt-images-v1", limits)
	require.ErrorIs(t, err, interruptedBeforeReceipt)
	partialPackage, err := s.LoadRetainedProductionPackage(t.Context(), job.ID, request.OperationID)
	require.NoError(t, err)
	_, err = s.LoadProductionReproduction(t.Context(), job.ID, request.OperationID)
	require.ErrorIs(t, err, ErrNotFound)
	staging, err := os.ReadDir(runtime.StagingDir)
	require.NoError(t, err)
	require.Empty(t, staging)
	_, err = runtime.Run(t.Context(), job.ID, request, policy, "export-dat-opt-images-v1", limits)
	require.ErrorIs(t, err, lostResponse)
	require.NoError(t, documentproduction.ValidateReproductionReceipt(first))
	require.Equal(t, request.OperationID, first.ID)
	require.Equal(t, originalReceipt.SHA256, first.OriginalProductionReceiptSHA256)
	require.Equal(t, originalReceipt.NumberReservationSHA256, first.OriginalNumberReservationSHA256)
	require.Equal(t, originalManifest.SHA256, first.ArtifactManifestSHA256)
	require.Zero(t, first.NumberAllocationCount)
	reproduced, err := runtime.Run(t.Context(), job.ID, request, policy, "export-dat-opt-images-v1", limits)
	require.NoError(t, err)
	require.Equal(t, sourceVersionIDs, reproduced.Selection.SourceVersionIDs)
	require.Equal(t, first, second)
	require.Equal(t, firstDelivery, secondDelivery)
	require.Equal(t, first.DeliveryPolicySHA256, firstDelivery.Receipt.DeliveryPolicySHA256)
	loadedDelivery, err := s.LoadProductionReproductionDelivery(t.Context(), job.ID,
		request.OperationID, deliveryOperationID)
	require.NoError(t, err)
	require.Equal(t, firstDelivery, loadedDelivery)
	deliveryRaw, err := canonical.Marshal(firstDelivery)
	require.NoError(t, err)
	require.NotContains(t, string(deliveryRaw), proofPath)
	loaded, err := s.LoadProductionReproduction(t.Context(), job.ID, request.OperationID)
	require.NoError(t, err)
	require.Equal(t, first, loaded)
	lookupAfterReproduction, err := s.FindPublishedProductionNumber(t.Context(), label)
	require.NoError(t, err)
	require.Equal(t, originalNumber, lookupAfterReproduction,
		"a reproduced package must not replace the number's original artifact")
	require.Equal(t, loaded.OriginalProductionReceiptSHA256, lookupAfterReproduction.ProductionReceiptSHA256)
	require.Equal(t, loaded.ArtifactManifestSHA256, lookupAfterReproduction.ArtifactManifestSHA256)
	retained, err := s.LoadRetainedProductionPackage(t.Context(), job.ID, request.OperationID)
	require.NoError(t, err)
	require.Equal(t, partialPackage, retained, "retry must reuse the already retained package")
	require.Equal(t, retained.Evidence.QCSHA256, first.PackageQCSHA256)
	require.Equal(t, retained.QC.Version.BlobHash, firstDelivery.Receipt.PackageQCSHA256)
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
	restoredDelivery, err := restored.LoadProductionReproductionDelivery(t.Context(), job.ID,
		request.OperationID, deliveryOperationID)
	require.NoError(t, err)
	require.Equal(t, firstDelivery, restoredDelivery)
	_, err = s.db.ExecContext(t.Context(), `DROP TRIGGER production_operation_receipts_immutable_update`)
	require.NoError(t, err)
	detached := firstDelivery
	detached.JobID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	detachedRaw, err := canonical.Marshal(detached)
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `UPDATE production_operation_receipts
		SET response_json=?,response_sha256=? WHERE operation_id=?`,
		detachedRaw, digestProductionBytes(detachedRaw), deliveryOperationID)
	require.NoError(t, err)
	require.Error(t, s.ValidateMetadata(t.Context()))
}
