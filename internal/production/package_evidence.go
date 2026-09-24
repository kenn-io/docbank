package production

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/canonical"
)

const PackageEvidenceContractV1 = "production-package-evidence/v1"

var ErrPackageEvidence = errors.New("production package evidence conflicts with published authority")

// PackageEvidenceReceipt identifies aggregate recipient evidence by its
// manifest and archive. A package combines occurrences, so it has no single
// source version or member identity.
type PackageEvidenceReceipt struct {
	Contract                string `json:"contract"`
	ID                      string `json:"id"`
	JobID                   string `json:"job_id"`
	ProductionReceiptSHA256 string `json:"production_receipt_sha256"`
	ArtifactManifestSHA256  string `json:"artifact_manifest_sha256"`
	RecipientManifestSHA256 string `json:"recipient_manifest_sha256"`
	ArchiveSHA256           string `json:"archive_sha256"`
	QCSHA256                string `json:"qc_sha256"`
	ProfileID               string `json:"profile_id"`
	SHA256                  string `json:"sha256"`
}

// BuildPackageEvidenceReceipt binds verified archive QC to the exact published
// production and its recipient projection. The archive bytes must also pass
// VerifyRecipientArchiveWithQC before the receipt is used for retention.
func BuildPackageEvidenceReceipt(job Job, projection PackageProjection, qc PackageQC,
	operationID string) (PackageEvidenceReceipt, error) {
	if !canonicalUUIDv4(operationID) || !canonicalUUIDv4(job.ID) ||
		job.State != ProductionJobSucceeded || job.Receipt.ID != job.ID || job.Receipt.JobID != job.ID ||
		job.Receipt.SetID != job.SetID || job.Receipt.Revision != job.Revision ||
		job.Receipt.RevisionSHA256 != job.RevisionSHA256 ||
		job.Receipt.PreparedInputSHA256 != job.PreparedInputSHA256 ||
		job.Receipt.ArtifactManifestSHA256 != job.Manifest.SHA256 ||
		documentproduction.ValidateProductionReceipt(job.Receipt) != nil ||
		documentproduction.ValidateArtifactManifest(job.Manifest) != nil ||
		projection.jobID != job.ID || projection.manifestSHA256 == "" ||
		qc.Contract != PackageQCContractV1 || qc.ManifestSHA256 != projection.manifestSHA256 ||
		!canonicalSHA256(qc.ArchiveSHA256) || len(qc.Entries) == 0 ||
		!slices.Equal(qc.PageNumbers, projection.PageNumbers()) {
		return PackageEvidenceReceipt{}, ErrPackageEvidence
	}
	manifestRaw, err := canonical.Marshal(projection.Manifest)
	if err != nil || len(manifestRaw) > maxPackageMetadataBytes {
		return PackageEvidenceReceipt{}, ErrPackageEvidence
	}
	manifestDigest := sha256.Sum256(manifestRaw)
	if hex.EncodeToString(manifestDigest[:]) != qc.ManifestSHA256 {
		return PackageEvidenceReceipt{}, ErrPackageEvidence
	}
	expectedPaths, err := packageExpectedPaths(projection.Manifest)
	if err != nil || len(qc.Entries) != len(expectedPaths) {
		return PackageEvidenceReceipt{}, ErrPackageEvidence
	}
	entries := make(map[string]PackageQCEntry, len(qc.Entries))
	for _, entry := range qc.Entries {
		if _, ok := expectedPaths[entry.Path]; !ok || !canonicalSHA256(entry.SHA256) ||
			entry.Size < 0 || entries[entry.Path].Path != "" {
			return PackageEvidenceReceipt{}, ErrPackageEvidence
		}
		entries[entry.Path] = entry
	}
	for _, binding := range projection.bindings {
		entry, ok := entries[binding.path]
		if !ok || entry.SHA256 != binding.artifact.SHA256 || entry.Size != binding.artifact.Size {
			return PackageEvidenceReceipt{}, ErrPackageEvidence
		}
	}
	qcRaw, err := canonical.Marshal(qc)
	if err != nil || len(qcRaw) > maxPackageMetadataBytes {
		return PackageEvidenceReceipt{}, ErrPackageEvidence
	}
	qcDigest := sha256.Sum256(qcRaw)
	receipt := PackageEvidenceReceipt{
		Contract: PackageEvidenceContractV1, ID: operationID, JobID: job.ID,
		ProductionReceiptSHA256: job.Receipt.SHA256,
		ArtifactManifestSHA256:  job.Manifest.SHA256,
		RecipientManifestSHA256: qc.ManifestSHA256,
		ArchiveSHA256:           qc.ArchiveSHA256,
		QCSHA256:                hex.EncodeToString(qcDigest[:]),
		ProfileID:               projection.Manifest.ProfileID,
	}
	encoded, err := canonical.Marshal(receipt)
	if err != nil {
		return PackageEvidenceReceipt{}, ErrPackageEvidence
	}
	digest := sha256.Sum256(encoded)
	receipt.SHA256 = hex.EncodeToString(digest[:])
	return receipt, ValidatePackageEvidenceReceipt(receipt)
}

func ValidatePackageEvidenceReceipt(receipt PackageEvidenceReceipt) error {
	if receipt.Contract != PackageEvidenceContractV1 ||
		!canonicalUUIDv4(receipt.ID) || !canonicalUUIDv4(receipt.JobID) ||
		!canonicalSHA256(receipt.ProductionReceiptSHA256) ||
		!canonicalSHA256(receipt.ArtifactManifestSHA256) ||
		!canonicalSHA256(receipt.RecipientManifestSHA256) ||
		!canonicalSHA256(receipt.ArchiveSHA256) || !canonicalSHA256(receipt.QCSHA256) ||
		!canonicalSHA256(receipt.SHA256) ||
		receipt.ProfileID != "export-dat-pdf-v1" &&
			receipt.ProfileID != "export-dat-opt-images-v1" &&
			receipt.ProfileID != "export-dat-lfp-images-v1" {
		return ErrPackageEvidence
	}
	expected := receipt
	expected.SHA256 = ""
	encoded, err := canonical.Marshal(expected)
	if err != nil {
		return ErrPackageEvidence
	}
	digest := sha256.Sum256(encoded)
	if hex.EncodeToString(digest[:]) != receipt.SHA256 {
		return ErrPackageEvidence
	}
	return nil
}
