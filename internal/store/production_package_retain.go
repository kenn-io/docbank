package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"

	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/production"
)

// ProductionPackageBlobWriter durably writes exact bytes before their metadata
// is published. Its physical receipt must describe the completed blob write.
type ProductionPackageBlobWriter func(context.Context, io.Reader) (string, int64, BlobPhysical, error)

type RetainedProductionPackage struct {
	Evidence    production.PackageEvidenceReceipt
	Archive     ContentWriteReceipt
	QC          ContentWriteReceipt
	Transmittal ContentWriteReceipt
}

// RetainProductionPackage binds a verified recipient handoff to a completed
// job, then imports the archive and both external sidecars as immutable vault
// versions. Repeating the same operation reconciles existing exact writes.
func (s *Store) RetainProductionPackage(ctx context.Context, jobID, operationID, profileID string,
	limits production.PackageLimits, archivePath, qcPath, transmittalPath string,
	write ProductionPackageBlobWriter) (RetainedProductionPackage, error) {
	return s.retainProductionPackage(ctx, jobID, operationID, profileID, limits,
		archivePath, qcPath, transmittalPath, write, nil)
}

// afterWrite injects a lost response after a durable package mutation in tests.
func (s *Store) retainProductionPackage(ctx context.Context, jobID, operationID, profileID string,
	limits production.PackageLimits, archivePath, qcPath, transmittalPath string,
	write ProductionPackageBlobWriter, afterWrite func(string) error) (RetainedProductionPackage, error) {
	bad := func(err error) (RetainedProductionPackage, error) {
		return RetainedProductionPackage{}, errors.Join(production.ErrPackageEvidence, err)
	}
	written := func(stage string) error {
		if afterWrite != nil {
			return afterWrite(stage)
		}
		return nil
	}
	if ctx == nil || validateUUIDv4(jobID) != nil || validateUUIDv4(operationID) != nil ||
		write == nil || archivePath == "" || qcPath == "" || transmittalPath == "" ||
		filepath.Clean(archivePath) == filepath.Clean(qcPath) ||
		filepath.Clean(archivePath) == filepath.Clean(transmittalPath) ||
		filepath.Clean(qcPath) == filepath.Clean(transmittalPath) {
		return bad(nil)
	}
	inputs, err := s.LoadProductionPackageInputs(ctx, jobID)
	if err != nil {
		return bad(err)
	}
	projection, err := production.PlanPackageProjection(inputs.Job, inputs.Reservation, inputs.Members, profileID, limits)
	if err != nil {
		return bad(err)
	}
	qc, err := production.ReadPackageQCReceipt(qcPath)
	if err != nil {
		return bad(err)
	}
	transmittal, err := production.ReadRecipientTransmittal(transmittalPath)
	if err != nil {
		return bad(err)
	}
	if err := production.VerifyRecipientArchiveWithQCContext(ctx, archivePath, qc); err != nil {
		return bad(err)
	}
	if err := production.PublishRecipientTransmittalContext(ctx, archivePath, transmittalPath,
		projection.Manifest, qc); err != nil {
		return bad(err)
	}
	evidence, err := production.BuildPackageEvidenceReceipt(inputs.Job, projection, qc, operationID)
	if err != nil {
		return bad(err)
	}
	qcBytes, err := canonical.Marshal(qc)
	if err != nil {
		return bad(err)
	}
	transmittalBytes, err := canonical.Marshal(transmittal)
	if err != nil {
		return bad(err)
	}
	qcSum := sha256.Sum256(append(qcBytes, '\n'))
	transmittalSum := sha256.Sum256(append(transmittalBytes, '\n'))
	parent, err := s.MkdirAll(ctx, "/productions/"+jobID+"/packages/"+operationID)
	if err != nil {
		return bad(err)
	}
	if err := written("directory"); err != nil {
		return bad(err)
	}
	parts := []struct {
		name, path, mime, expectedSHA string
	}{
		{"recipient.zip", archivePath, "application/zip", qc.ArchiveSHA256},
		{"qc.json", qcPath, "application/json", hex.EncodeToString(qcSum[:])},
		{"transmittal.json", transmittalPath, "application/json", hex.EncodeToString(transmittalSum[:])},
	}
	var retained [3]ContentWriteReceipt
	for index, part := range parts {
		retained[index], err = s.retainProductionPackagePart(ctx, parent.ID, evidence,
			part.name, part.path, part.mime, part.expectedSHA, write, written)
		if err != nil {
			return bad(err)
		}
	}
	return RetainedProductionPackage{Evidence: evidence, Archive: retained[0], QC: retained[1],
		Transmittal: retained[2]}, nil
}

func (s *Store) retainProductionPackagePart(ctx context.Context, parentID int64,
	evidence production.PackageEvidenceReceipt, name, sourcePath, mime, expectedSHA string,
	write ProductionPackageBlobWriter, afterWrite func(string) error) (ContentWriteReceipt, error) {
	file, err := os.Open(sourcePath)
	if err != nil {
		return ContentWriteReceipt{}, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 {
		return ContentWriteReceipt{}, production.ErrPackageEvidence
	}
	hash, size, physical, err := write(ctx, packageRetainContextReader{ctx: ctx, reader: file})
	if err != nil {
		return ContentWriteReceipt{}, err
	}
	if hash != expectedSHA || size != info.Size() {
		return ContentWriteReceipt{}, production.ErrPackageEvidence
	}
	if err := afterWrite(name + ":write"); err != nil {
		return ContentWriteReceipt{}, err
	}
	if err := s.RecordBlob(ctx, hash, size, physical); err != nil {
		return ContentWriteReceipt{}, err
	}
	if err := afterWrite(name + ":blob"); err != nil {
		return ContentWriteReceipt{}, err
	}
	description, err := canonical.Marshal(struct {
		Contract string                            `json:"contract"`
		Evidence production.PackageEvidenceReceipt `json:"evidence"`
		Part     string                            `json:"part"`
	}{"production-retained-package/v1", evidence, name})
	if err != nil {
		return ContentWriteReceipt{}, err
	}
	path := "/productions/" + evidence.JobID + "/packages/" + evidence.ID + "/" + name
	node, err := s.NodeByPath(ctx, path)
	if errors.Is(err, ErrNotFound) {
		run, runErr := s.BeginCallerSuppliedIngest(ctx, "production-package", string(description))
		if runErr != nil {
			return ContentWriteReceipt{}, runErr
		}
		receipt, ingestErr := s.IngestFileExactWithReceipt(ctx, run, parentID, name,
			hash, size, mime, name, "", physical)
		if ingestErr == nil {
			if err := afterWrite(name + ":ingest"); err != nil {
				return ContentWriteReceipt{}, err
			}
			return receipt, nil
		}
		if !errors.Is(ingestErr, ErrExists) {
			return ContentWriteReceipt{}, ingestErr
		}
		node, err = s.NodeByPath(ctx, path)
	}
	if err != nil || node.IsDir() || node.TrashedAt != nil ||
		node.BlobHash != hash || node.Size != size || node.MimeType != mime {
		return ContentWriteReceipt{}, errors.Join(production.ErrPackageEvidence, err)
	}
	var matches int
	var versionID string
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MIN(b.content_version_id),'')
		FROM provenance p JOIN ingests i ON i.id=p.ingest_id
		JOIN provenance_version_bindings b ON b.provenance_identity=p.identity
		WHERE p.node_id=? AND i.source_kind='embedded:production-package' AND i.source_desc=?
		AND p.original_path=? AND b.basis_ref=?`, node.ID, string(description), name,
		provenanceVersionBindingBasis).Scan(&matches, &versionID)
	if err != nil || matches != 1 || versionID != node.CurrentVersionID {
		return ContentWriteReceipt{}, errors.Join(production.ErrPackageEvidence, err)
	}
	version, err := s.ContentVersionByID(ctx, versionID)
	if err != nil || version.NodeID != node.ID || version.BlobHash != hash ||
		version.Size != size || version.MimeType != mime {
		return ContentWriteReceipt{}, errors.Join(production.ErrPackageEvidence, err)
	}
	physicalContent, err := s.PhysicalContent(ctx, hash)
	if err != nil {
		return ContentWriteReceipt{}, err
	}
	return ContentWriteReceipt{Node: node, Version: version, Physical: physicalContent}, nil
}

type packageRetainContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r packageRetainContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
