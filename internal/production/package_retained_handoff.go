package production

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"go.kenn.io/docbank/internal/canonical"
)

// VerifyRetainedPackageHandoffContext independently checks the staged physical
// archive and sidecars against the aggregate evidence before a download may be
// offered. The caller must first verify each staged byte stream against its
// vault version hash and size.
func VerifyRetainedPackageHandoffContext(ctx context.Context, archivePath, qcPath, transmittalPath string,
	evidence PackageEvidenceReceipt) error {
	if ctx == nil || ValidatePackageEvidenceReceipt(evidence) != nil ||
		archivePath == "" || qcPath == "" || transmittalPath == "" {
		return ErrPackageEvidence
	}
	qc, _, _, err := packageHandoffInputsContext(ctx, archivePath, qcPath, transmittalPath)
	if err != nil {
		return err
	}
	qcRaw, err := canonical.Marshal(qc)
	if err != nil {
		return ErrPackageEvidence
	}
	qcHash := sha256.Sum256(qcRaw)
	manifest, err := recipientManifestFromVerifiedArchiveContext(ctx, archivePath, qc.ManifestSHA256)
	if err != nil {
		return err
	}
	if qc.ArchiveSHA256 != evidence.ArchiveSHA256 || qc.ManifestSHA256 != evidence.RecipientManifestSHA256 ||
		hex.EncodeToString(qcHash[:]) != evidence.QCSHA256 || manifest.ProfileID != evidence.ProfileID {
		return ErrPackageEvidence
	}
	return nil
}
