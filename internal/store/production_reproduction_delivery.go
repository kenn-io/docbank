package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"time"

	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/production"
)

const (
	productionOperationReproductionDelivery = "reproduction_delivery"
	reproductionDeliveryContractV1          = "production-reproduction-delivery/v1"
)

// ReproductionDeliveryRecord binds a caller-supplied delivery acknowledgment
// to one retained reproduction. It records the proof digest, never its path.
type ReproductionDeliveryRecord struct {
	Contract       string                            `json:"contract"`
	ID             string                            `json:"id"`
	JobID          string                            `json:"job_id"`
	ReproductionID string                            `json:"reproduction_id"`
	Receipt        production.PackageDeliveryReceipt `json:"receipt"`
}

func validReproductionDeliveryRecord(value ReproductionDeliveryRecord) bool {
	receipt := value.Receipt
	if value.Contract != reproductionDeliveryContractV1 || validateUUIDv4(value.ID) != nil ||
		validateUUIDv4(value.JobID) != nil || validateUUIDv4(value.ReproductionID) != nil ||
		receipt.Contract != production.PackageDeliveryReceiptContractV1 ||
		!canonical.IsSHA256Hex(receipt.ArchiveSHA256) || !canonical.IsSHA256Hex(receipt.ManifestSHA256) ||
		!canonical.IsSHA256Hex(receipt.PackageQCSHA256) || !canonical.IsSHA256Hex(receipt.TransmittalSHA256) ||
		!canonical.IsSHA256Hex(receipt.DeliveryPolicySHA256) || !canonical.IsSHA256Hex(receipt.ProofSHA256) ||
		receipt.ProofBytes < 1 || receipt.RecipientCode == "" || len(receipt.RecipientCode) > 128 ||
		(receipt.Method != "secure-transfer" && receipt.Method != "offline-media") {
		return false
	}
	for _, char := range receipt.RecipientCode {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || char == '-' || char == '_' {
			continue
		}
		return false
	}
	when, err := time.Parse(time.RFC3339, receipt.DeliveredAt)
	return err == nil && when.UTC().Format(time.RFC3339) == receipt.DeliveredAt
}

func reproductionDeliveryPartNames(operationID string) (string, string) {
	return "delivery-" + operationID + ".json", "delivery-" + operationID + ".proof"
}

// RecordProductionReproductionDelivery verifies the package and supplied
// evidence, retains both the receipt and proof as private vault versions, then
// records one immutable operation. Retrying after any retained part reconciles
// the exact bytes before the operation can be returned.
func (s *Store) RecordProductionReproductionDelivery(ctx context.Context,
	jobID, reproductionID, operationID string, policy production.PackageDeliveryPolicy,
	evidence production.PackageDeliveryEvidence, paths production.RecipientPackageRequest,
	receiptPath string, write ProductionPackageBlobWriter) (ReproductionDeliveryRecord, error) {
	bad := func() (ReproductionDeliveryRecord, error) {
		return ReproductionDeliveryRecord{}, production.ErrReproductionConflict
	}
	if ctx == nil || validateUUIDv4(operationID) != nil || operationID == reproductionID ||
		paths.JobID != jobID || receiptPath == "" || write == nil {
		return bad()
	}
	reproduction, err := s.LoadProductionReproduction(ctx, jobID, reproductionID)
	if err != nil {
		return bad()
	}
	retained, err := s.LoadRetainedProductionPackage(ctx, jobID, reproductionID)
	if err != nil {
		return bad()
	}
	policySHA, err := production.PackageDeliveryPolicySHA256(policy)
	if err != nil || policySHA != reproduction.DeliveryPolicySHA256 {
		return bad()
	}
	binding := production.PackageDeliveryBinding{
		ArchiveSHA256:     retained.Evidence.ArchiveSHA256,
		ManifestSHA256:    retained.Evidence.RecipientManifestSHA256,
		PackageQCSHA256:   retained.QC.Version.BlobHash,
		TransmittalSHA256: retained.Transmittal.Version.BlobHash,
	}
	receipt, err := production.RecordPackageDeliveryBound(paths.ArchivePath, paths.QCPath,
		paths.TransmittalPath, receiptPath, policy, evidence, binding)
	if err != nil || receipt.DeliveryPolicySHA256 != reproduction.DeliveryPolicySHA256 ||
		receipt.ArchiveSHA256 != retained.Evidence.ArchiveSHA256 ||
		receipt.ManifestSHA256 != retained.Evidence.RecipientManifestSHA256 ||
		receipt.PackageQCSHA256 != retained.QC.Version.BlobHash ||
		receipt.TransmittalSHA256 != retained.Transmittal.Version.BlobHash {
		return bad()
	}
	result := ReproductionDeliveryRecord{Contract: reproductionDeliveryContractV1,
		ID: operationID, JobID: jobID, ReproductionID: reproductionID, Receipt: receipt}
	if !validReproductionDeliveryRecord(result) {
		return bad()
	}
	receiptRaw, err := canonical.Marshal(receipt)
	if err != nil {
		return bad()
	}
	parent, err := s.MkdirAll(ctx, "/productions/"+jobID+"/packages/"+reproductionID)
	if err != nil {
		return bad()
	}
	receiptName, proofName := reproductionDeliveryPartNames(operationID)
	afterWrite := func(string) error { return nil }
	if _, err := s.retainProductionPackagePart(ctx, parent.ID, retained.Evidence,
		receiptName, receiptPath, "application/json", digestProductionBytes(append(receiptRaw, '\n')),
		write, afterWrite); err != nil {
		return bad()
	}
	if _, err := s.retainProductionPackagePart(ctx, parent.ID, retained.Evidence,
		proofName, evidence.ProofPath, "application/octet-stream", receipt.ProofSHA256,
		write, afterWrite); err != nil {
		return bad()
	}
	requestRaw, err := canonical.Marshal(result)
	if err != nil {
		return bad()
	}
	requestDigest := digestProductionBytes(requestRaw)
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		prior, replay, err := productionOperationReplay[ReproductionDeliveryRecord](ctx, tx,
			operationID, productionOperationReproductionDelivery, requestDigest)
		if err != nil {
			return err
		}
		if replay {
			if !validReproductionDeliveryRecord(prior) || prior != result {
				return production.ErrReproductionConflict
			}
			result = prior
			return nil
		}
		return recordProductionOperation(ctx, tx, operationID,
			productionOperationReproductionDelivery, requestDigest, result)
	})
	if err != nil {
		return bad()
	}
	return result, nil
}

// LoadProductionReproductionDelivery reopens a delivery only when its original
// reproduction, package, private receipt sidecar, and proof remain linked.
func (s *Store) LoadProductionReproductionDelivery(ctx context.Context,
	jobID, reproductionID, operationID string) (ReproductionDeliveryRecord, error) {
	bad := func() (ReproductionDeliveryRecord, error) {
		return ReproductionDeliveryRecord{}, production.ErrReproductionConflict
	}
	if ctx == nil || validateUUIDv4(jobID) != nil || validateUUIDv4(reproductionID) != nil ||
		validateUUIDv4(operationID) != nil {
		return bad()
	}
	var kind, responseSHA string
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT kind,response_sha256,response_json
		FROM production_operation_receipts WHERE operation_id=?`, operationID).
		Scan(&kind, &responseSHA, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return ReproductionDeliveryRecord{}, ErrNotFound
	}
	if err != nil || kind != productionOperationReproductionDelivery || digestProductionBytes(raw) != responseSHA {
		return bad()
	}
	result, err := canonical.Decode[ReproductionDeliveryRecord](raw)
	if err != nil || !validReproductionDeliveryRecord(result) || result.ID != operationID ||
		result.JobID != jobID || result.ReproductionID != reproductionID {
		return bad()
	}
	encoded, err := canonical.Marshal(result)
	if err != nil || !bytes.Equal(encoded, raw) {
		return bad()
	}
	reproduction, err := s.LoadProductionReproduction(ctx, jobID, reproductionID)
	if err != nil || result.Receipt.DeliveryPolicySHA256 != reproduction.DeliveryPolicySHA256 {
		return bad()
	}
	retained, err := s.LoadRetainedProductionPackage(ctx, jobID, reproductionID)
	if err != nil || result.Receipt.ArchiveSHA256 != retained.Evidence.ArchiveSHA256 ||
		result.Receipt.ManifestSHA256 != retained.Evidence.RecipientManifestSHA256 ||
		result.Receipt.PackageQCSHA256 != retained.QC.Version.BlobHash ||
		result.Receipt.TransmittalSHA256 != retained.Transmittal.Version.BlobHash {
		return bad()
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return bad()
	}
	defer func() { _ = tx.Rollback() }()
	base := "/productions/" + jobID + "/packages/" + reproductionID + "/"
	receiptName, proofName := reproductionDeliveryPartNames(operationID)
	receiptPart, receiptEvidence, err := s.loadRetainedPackagePart(ctx, tx, base,
		receiptName, "application/json")
	if err != nil || receiptEvidence != retained.Evidence {
		return bad()
	}
	proofPart, proofEvidence, err := s.loadRetainedPackagePart(ctx, tx, base,
		proofName, "application/octet-stream")
	if err != nil || proofEvidence != retained.Evidence {
		return bad()
	}
	receiptRaw, err := canonical.Marshal(result.Receipt)
	if err != nil || receiptPart.Version.BlobHash != digestProductionBytes(append(receiptRaw, '\n')) ||
		proofPart.Version.BlobHash != result.Receipt.ProofSHA256 ||
		proofPart.Version.Size != result.Receipt.ProofBytes {
		return bad()
	}
	if err := tx.Commit(); err != nil {
		return bad()
	}
	return result, nil
}
