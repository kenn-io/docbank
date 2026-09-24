package store

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/production"
)

const maxRetainedPackageProvenanceBytes = 8 << 10

type retainedPackageProvenance struct {
	Contract string                            `json:"contract"`
	Evidence production.PackageEvidenceReceipt `json:"evidence"`
	Part     string                            `json:"part"`
}

// LoadRetainedProductionPackage reopens the three current vault versions and
// their common immutable package evidence from one read snapshot. Callers must
// still verify physical bytes when serving a download.
func (s *Store) LoadRetainedProductionPackage(ctx context.Context, jobID, operationID string) (
	RetainedProductionPackage, error,
) {
	bad := func(err error) (RetainedProductionPackage, error) {
		return RetainedProductionPackage{}, errors.Join(production.ErrPackageEvidence, err)
	}
	if ctx == nil || validateUUIDv4(jobID) != nil || validateUUIDv4(operationID) != nil {
		return bad(nil)
	}
	job, err := s.LoadProductionJob(ctx, jobID)
	if err != nil {
		return RetainedProductionPackage{}, err
	}
	if job.State != production.ProductionJobSucceeded {
		return bad(nil)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return RetainedProductionPackage{}, err
	}
	defer func() { _ = tx.Rollback() }()
	base := "/productions/" + jobID + "/packages/" + operationID + "/"
	archive, evidence, err := s.loadRetainedPackagePart(ctx, tx, base, "recipient.zip", "application/zip")
	if err != nil {
		return RetainedProductionPackage{}, err
	}
	if evidence.ID != operationID || evidence.JobID != jobID ||
		evidence.ProductionReceiptSHA256 != job.Receipt.SHA256 ||
		evidence.ArtifactManifestSHA256 != job.Manifest.SHA256 ||
		evidence.ArchiveSHA256 != archive.Version.BlobHash {
		return bad(nil)
	}
	qc, qcEvidence, err := s.loadRetainedPackagePart(ctx, tx, base, "qc.json", "application/json")
	if err != nil || qcEvidence != evidence {
		return bad(err)
	}
	transmittal, transmittalEvidence, err := s.loadRetainedPackagePart(ctx, tx, base, "transmittal.json", "application/json")
	if err != nil || transmittalEvidence != evidence {
		return bad(err)
	}
	if err := tx.Commit(); err != nil {
		return RetainedProductionPackage{}, err
	}
	return RetainedProductionPackage{Evidence: evidence, Archive: archive, QC: qc,
		Transmittal: transmittal}, nil
}

func (s *Store) loadRetainedPackagePart(ctx context.Context, tx *sql.Tx, base, name, mime string) (
	ContentWriteReceipt, production.PackageEvidenceReceipt, error,
) {
	node, err := nodeByPath(ctx, tx, s.rootID, base+name)
	if err != nil {
		return ContentWriteReceipt{}, production.PackageEvidenceReceipt{}, err
	}
	bad := func(err error) (ContentWriteReceipt, production.PackageEvidenceReceipt, error) {
		return ContentWriteReceipt{}, production.PackageEvidenceReceipt{}, errors.Join(production.ErrPackageEvidence, err)
	}
	if node.IsDir() || node.TrashedAt != nil || node.CurrentVersionID == "" || node.MimeType != mime {
		return bad(nil)
	}
	version, err := scanContentVersion(tx.QueryRowContext(ctx,
		`SELECT `+contentVersionCols+` FROM content_versions WHERE version_id=? AND node_id=?`,
		node.CurrentVersionID, node.ID))
	if err != nil || version.BlobHash != node.BlobHash || version.Size != node.Size ||
		version.MimeType != mime {
		return bad(err)
	}
	var count, sourceLength int
	var description string
	var originalPath, basis string
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(length(i.source_desc)),0),
		COALESCE(MAX(substr(i.source_desc,1,8193)),''),COALESCE(MAX(p.original_path),''),COALESCE(MAX(b.basis_ref),'')
		FROM provenance p JOIN ingests i ON i.id=p.ingest_id
		JOIN provenance_version_bindings b ON b.provenance_identity=p.identity
		WHERE p.node_id=? AND b.content_version_id=? AND i.source_kind='embedded:production-package'`,
		node.ID, version.ID).Scan(&count, &sourceLength, &description, &originalPath, &basis)
	if err != nil || count != 1 || sourceLength < 1 || sourceLength > maxRetainedPackageProvenanceBytes ||
		originalPath != name || basis != provenanceVersionBindingBasis {
		return bad(err)
	}
	provenance, err := canonical.Decode[retainedPackageProvenance]([]byte(description))
	if err != nil || provenance.Contract != "production-retained-package/v1" ||
		provenance.Part != name || production.ValidatePackageEvidenceReceipt(provenance.Evidence) != nil {
		return bad(err)
	}
	physical, err := authorizedPhysicalContentTx(tx, version.BlobHash)
	if err != nil || physical.LogicalBytes != version.Size {
		return bad(err)
	}
	return ContentWriteReceipt{Node: node, Version: version, Physical: physical}, provenance.Evidence, nil
}
