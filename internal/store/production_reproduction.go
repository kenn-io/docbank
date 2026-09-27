package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/production"
)

const productionOperationReproduction = "reproduction"

// RetainProductionReproduction keeps a new verified archive and a separate
// immutable reproduction receipt under the request's operation identity. The
// package files may be retained before the receipt transaction; retrying the
// same operation reconciles those exact files and records one receipt.
func (s *Store) RetainProductionReproduction(ctx context.Context, jobID string,
	request documentproduction.ReproductionRequest, policy production.PackageDeliveryPolicy,
	paths production.RecipientPackageRequest, opener production.PackageArtifactOpener,
	write ProductionPackageBlobWriter) (documentproduction.ReproductionReceipt, error) {
	bad := func() (documentproduction.ReproductionReceipt, error) {
		return documentproduction.ReproductionReceipt{}, production.ErrReproductionConflict
	}
	if ctx == nil || paths.JobID != jobID || paths.ProfileID == "" ||
		paths.ArchivePath == "" || paths.QCPath == "" || paths.TransmittalPath == "" || write == nil {
		return bad()
	}
	_, requestSHA, err := documentproduction.CanonicalReproductionRequest(request)
	if err != nil {
		return bad()
	}
	requestRaw, err := canonical.Marshal(struct {
		Contract           string `json:"contract"`
		RequestSHA256      string `json:"request_sha256"`
		ProfileID          string `json:"profile_id"`
		MaxVolumeBytes     int64  `json:"max_volume_bytes"`
		MaxVolumeDocuments int    `json:"max_volume_documents"`
	}{"production-reproduction-operation/v1", requestSHA, paths.ProfileID,
		paths.Limits.MaxVolumeBytes, paths.Limits.MaxVolumeDocuments})
	if err != nil {
		return bad()
	}
	requestDigest := digestProductionBytes(requestRaw)
	selection, err := production.PrepareReproduction(ctx, s, opener, jobID, request, policy)
	if err != nil {
		return bad()
	}
	retained, err := s.RetainProductionPackage(ctx, jobID, request.OperationID,
		paths.ProfileID, paths.Limits, paths.ArchivePath, paths.QCPath,
		paths.TransmittalPath, write)
	if err != nil {
		return bad()
	}
	if retained.Evidence.ID != request.OperationID || retained.Evidence.JobID != jobID ||
		retained.Evidence.ProductionReceiptSHA256 != selection.OriginalReceiptSHA256 ||
		retained.Evidence.ArtifactManifestSHA256 != selection.ArtifactManifestSHA256 ||
		retained.Evidence.ProfileID != paths.ProfileID {
		return bad()
	}
	var result documentproduction.ReproductionReceipt
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		prior, replay, err := productionOperationReplay[documentproduction.ReproductionReceipt](ctx, tx,
			request.OperationID, productionOperationReproduction, requestDigest)
		if err != nil {
			return err
		}
		if replay {
			if documentproduction.ValidateReproductionReceipt(prior) != nil ||
				prior.ID != request.OperationID || prior.OriginalProductionReceiptSHA256 != selection.OriginalReceiptSHA256 ||
				prior.OriginalNumberReservationSHA256 != selection.OriginalReservationSHA256 ||
				prior.ArtifactManifestSHA256 != selection.ArtifactManifestSHA256 ||
				prior.PackageQCSHA256 != retained.Evidence.QCSHA256 ||
				prior.DeliveryPolicySHA256 != selection.DeliveryPolicySHA256 {
				return production.ErrReproductionConflict
			}
			result = prior
			return nil
		}
		result = documentproduction.ReproductionReceipt{
			Contract:                        documentproduction.ReproductionReceiptContractV1,
			ID:                              request.OperationID,
			OriginalProductionReceiptSHA256: selection.OriginalReceiptSHA256,
			OriginalNumberReservationSHA256: selection.OriginalReservationSHA256,
			ArtifactManifestSHA256:          selection.ArtifactManifestSHA256,
			PackageQCSHA256:                 retained.Evidence.QCSHA256,
			DeliveryPolicySHA256:            selection.DeliveryPolicySHA256,
			NumberAllocationCount:           0,
			CreatedAt:                       nowRFC3339(),
		}
		_, digest, err := documentproduction.CanonicalReproductionReceipt(result)
		if err != nil {
			return err
		}
		result.SHA256 = digest
		return recordProductionOperation(ctx, tx, request.OperationID,
			productionOperationReproduction, requestDigest, result)
	})
	if err != nil {
		return bad()
	}
	return result, nil
}

// LoadProductionReproduction verifies the separate receipt against its
// original job and retained package before returning a historical link.
func (s *Store) LoadProductionReproduction(ctx context.Context, jobID, operationID string) (
	documentproduction.ReproductionReceipt, error,
) {
	bad := func() (documentproduction.ReproductionReceipt, error) {
		return documentproduction.ReproductionReceipt{}, production.ErrReproductionConflict
	}
	if ctx == nil || validateUUIDv4(jobID) != nil || validateUUIDv4(operationID) != nil {
		return bad()
	}
	var kind, responseSHA string
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT kind,response_sha256,response_json
		FROM production_operation_receipts WHERE operation_id=?`, operationID).
		Scan(&kind, &responseSHA, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return documentproduction.ReproductionReceipt{}, ErrNotFound
	}
	if err != nil || kind != productionOperationReproduction || digestProductionBytes(raw) != responseSHA {
		return bad()
	}
	receipt, err := canonical.Decode[documentproduction.ReproductionReceipt](raw)
	if err != nil || documentproduction.ValidateReproductionReceipt(receipt) != nil || receipt.ID != operationID {
		return bad()
	}
	canonicalRaw, err := canonical.Marshal(receipt)
	if err != nil || !bytes.Equal(raw, canonicalRaw) {
		return bad()
	}
	inputs, err := s.LoadProductionPackageInputs(ctx, jobID)
	if err != nil || inputs.Job.Receipt.SHA256 != receipt.OriginalProductionReceiptSHA256 ||
		inputs.Reservation.SHA256 != receipt.OriginalNumberReservationSHA256 ||
		inputs.Job.Manifest.SHA256 != receipt.ArtifactManifestSHA256 {
		return bad()
	}
	retained, err := s.LoadRetainedProductionPackage(ctx, jobID, operationID)
	if err != nil || retained.Evidence.ID != operationID ||
		retained.Evidence.QCSHA256 != receipt.PackageQCSHA256 ||
		retained.Evidence.ProductionReceiptSHA256 != receipt.OriginalProductionReceiptSHA256 ||
		retained.Evidence.ArtifactManifestSHA256 != receipt.ArtifactManifestSHA256 {
		return bad()
	}
	return receipt, nil
}
