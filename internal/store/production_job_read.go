package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/production"
)

const (
	maxProductionReceiptBytes      = 16 << 10
	maxProductionManifestBytes     = 128 << 20
	maxProductionEndorsementsBytes = 128 << 20
	maxProductionArtifactBytes     = 16 << 10
)

// LoadProductionJob returns a verified historical receipt after publication.
// A restarted worker uses it to resolve a lost publish acknowledgement.
func (s *Store) LoadProductionJob(ctx context.Context, jobID string) (production.Job, error) {
	if validateUUIDv4(jobID) != nil {
		return production.Job{}, production.ErrJobConflict
	}
	var job production.Job
	var receiptRaw, manifestRaw, endorsementsRaw []byte
	var receiptSize, manifestSize, endorsementsSize sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT length(receipt_json),length(artifact_manifest_json),length(endorsements_json)
		FROM production_jobs WHERE job_id=?`, jobID).Scan(&receiptSize, &manifestSize, &endorsementsSize)
	if errors.Is(err, sql.ErrNoRows) {
		return production.Job{}, ErrNotFound
	}
	if err != nil {
		return production.Job{}, err
	}
	if receiptSize.Int64 > maxProductionReceiptBytes || manifestSize.Int64 > maxProductionManifestBytes ||
		endorsementsSize.Int64 > maxProductionEndorsementsBytes {
		return production.Job{}, production.ErrJobConflict
	}
	err = s.db.QueryRowContext(ctx, `SELECT operation_id,set_id,revision,etag,prepared_input_sha256,revision_sha256,
		state,receipt_json,artifact_manifest_json,endorsements_json FROM production_jobs WHERE job_id=?`, jobID).
		Scan(&job.OperationID, &job.SetID, &job.Revision, &job.ETag, &job.PreparedInputSHA256,
			&job.RevisionSHA256, &job.State, &receiptRaw, &manifestRaw, &endorsementsRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return production.Job{}, ErrNotFound
	}
	if err != nil {
		return production.Job{}, err
	}
	job.ID = jobID
	if job.State != production.ProductionJobSucceeded {
		return job, nil
	}
	if len(receiptRaw) == 0 || len(receiptRaw) > maxProductionReceiptBytes ||
		len(manifestRaw) == 0 || len(manifestRaw) > maxProductionManifestBytes ||
		len(endorsementsRaw) == 0 || len(endorsementsRaw) > maxProductionEndorsementsBytes {
		return production.Job{}, production.ErrJobConflict
	}
	job.Receipt, err = canonical.Decode[documentproduction.ProductionReceipt](receiptRaw)
	if err != nil || documentproduction.ValidateProductionReceipt(job.Receipt) != nil ||
		job.Receipt.JobID != jobID || job.Receipt.SetID != job.SetID || job.Receipt.Revision != job.Revision ||
		job.Receipt.RevisionSHA256 != job.RevisionSHA256 || job.Receipt.PreparedInputSHA256 != job.PreparedInputSHA256 {
		return production.Job{}, production.ErrJobConflict
	}
	if raw, err := canonical.Marshal(job.Receipt); err != nil || !bytes.Equal(raw, receiptRaw) {
		return production.Job{}, production.ErrJobConflict
	}
	job.Manifest, err = canonical.Decode[documentproduction.ArtifactManifest](manifestRaw)
	if err != nil || documentproduction.ValidateArtifactManifest(job.Manifest) != nil ||
		job.Receipt.ArtifactManifestSHA256 != job.Manifest.SHA256 {
		return production.Job{}, production.ErrJobConflict
	}
	if raw, err := canonical.Marshal(job.Manifest); err != nil || !bytes.Equal(raw, manifestRaw) {
		return production.Job{}, production.ErrJobConflict
	}
	job.Endorsements, err = canonical.Decode[[]redaction.Endorsement](endorsementsRaw)
	if err != nil || job.Receipt.EndorsementsSHA256 != productionEndorsementDigest(job.Endorsements) {
		return production.Job{}, production.ErrJobConflict
	}
	if raw, err := canonical.Marshal(job.Endorsements); err != nil || !bytes.Equal(raw, endorsementsRaw) {
		return production.Job{}, production.ErrJobConflict
	}
	return job, nil
}

func productionEndorsementDigest(values []redaction.Endorsement) string {
	raw, err := canonical.Marshal(values)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// LoadProductionJobArtifact checks job-scoped metadata and physical catalog
// authority before a blob adapter may open its stream.
func (s *Store) LoadProductionJobArtifact(ctx context.Context, jobID, artifactID string) (documentproduction.Artifact, error) {
	if validateUUIDv4(jobID) != nil || validateUUIDv4(artifactID) != nil {
		return documentproduction.Artifact{}, production.ErrJobConflict
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return documentproduction.Artifact{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var size int64
	if err := tx.QueryRowContext(ctx, `SELECT length(artifact_json) FROM production_job_artifacts WHERE job_id=? AND artifact_id=?`,
		jobID, artifactID).Scan(&size); err != nil || size < 1 || size > maxProductionArtifactBytes {
		return documentproduction.Artifact{}, production.ErrJobConflict
	}
	var raw []byte
	var hash string
	var storedSize int64
	if err := tx.QueryRowContext(ctx, `SELECT artifact_sha256,artifact_size,artifact_json FROM production_job_artifacts WHERE job_id=? AND artifact_id=?`,
		jobID, artifactID).Scan(&hash, &storedSize, &raw); err != nil {
		return documentproduction.Artifact{}, errors.Join(production.ErrJobConflict, err)
	}
	if len(raw) > maxProductionArtifactBytes {
		return documentproduction.Artifact{}, production.ErrJobConflict
	}
	artifact, err := canonical.Decode[documentproduction.Artifact](raw)
	if err != nil || artifact.ID != artifactID || artifact.SHA256 != hash || artifact.Size != storedSize {
		return documentproduction.Artifact{}, production.ErrJobConflict
	}
	if _, _, err := documentproduction.CanonicalArtifactManifest(documentproduction.ArtifactManifest{
		Contract: documentproduction.ArtifactManifestContractV1, Artifacts: []documentproduction.Artifact{artifact},
	}); err != nil {
		return documentproduction.Artifact{}, production.ErrJobConflict
	}
	if canonicalRaw, err := canonical.Marshal(artifact); err != nil || !bytes.Equal(raw, canonicalRaw) {
		return documentproduction.Artifact{}, production.ErrJobConflict
	}
	size, err = requirePhysicalAuthorityTx(tx, artifact.SHA256)
	if err != nil || size != artifact.Size {
		return documentproduction.Artifact{}, production.ErrJobConflict
	}
	if err := tx.Commit(); err != nil {
		return documentproduction.Artifact{}, err
	}
	return artifact, nil
}
