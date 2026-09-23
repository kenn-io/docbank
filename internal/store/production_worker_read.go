package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/production"
)

const maxProductionJobRequestBytes = 4 << 10

// NextRunnableProductionJob reads the original bounded admission request.
// The single vault worker runs queued jobs and expired claims in stored order;
// a live claim owned by another worker is never selected for takeover.
func (s *Store) NextRunnableProductionJob(ctx context.Context) (production.JobRequest, bool, error) {
	var jobID string
	var size int64
	err := s.db.QueryRowContext(ctx, `SELECT job_id,length(request_json) FROM production_jobs
		WHERE cancel_requested=0 AND (state=? OR (state=? AND
			(lease_expires_at IS NULL OR julianday(lease_expires_at)<=julianday(?))))
		ORDER BY created_at,job_id LIMIT 1`, production.ProductionJobQueued,
		production.ProductionJobRunning, time.Now().UTC().Format(time.RFC3339Nano)).Scan(&jobID, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return production.JobRequest{}, false, nil
	}
	if err != nil {
		return production.JobRequest{}, false, err
	}
	if size < 1 || size > maxProductionJobRequestBytes {
		return production.JobRequest{}, false, production.ErrJobConflict
	}
	var raw []byte
	var operationID, setID, revisionSHA, preparedSHA string
	var revision, etag int64
	err = s.db.QueryRowContext(ctx, `SELECT request_json,operation_id,set_id,revision,etag,
		revision_sha256,prepared_input_sha256 FROM production_jobs WHERE job_id=?`, jobID).
		Scan(&raw, &operationID, &setID, &revision, &etag, &revisionSHA, &preparedSHA)
	if err != nil {
		return production.JobRequest{}, false, err
	}
	if len(raw) != int(size) || len(raw) > maxProductionJobRequestBytes {
		return production.JobRequest{}, false, production.ErrJobConflict
	}
	request, err := canonical.Decode[production.JobRequest](raw)
	if err != nil || request.JobID != jobID || request.OperationID != operationID ||
		request.SetID != setID || request.Revision != revision || request.ETag != etag ||
		request.RevisionSHA256 != revisionSHA || request.PreparedInputSHA256 != preparedSHA ||
		!canonical.IsSHA256Hex(request.NumberingProfileSHA256) {
		return production.JobRequest{}, false, production.ErrJobConflict
	}
	canonicalRaw, err := canonical.Marshal(request)
	if err != nil || !bytes.Equal(raw, canonicalRaw) {
		return production.JobRequest{}, false, production.ErrJobConflict
	}
	return request, true, nil
}

// LoadProductionRecipeForJob resolves the stored catalog recipe and checks it
// against the immutable prepared member authority before rendering.
func (s *Store) LoadProductionRecipeForJob(ctx context.Context, jobID string) (redaction.Recipe, error) {
	if validateUUIDv4(jobID) != nil {
		return redaction.Recipe{}, production.ErrJobConflict
	}
	job, err := s.LoadProductionJob(ctx, jobID)
	if err != nil {
		return redaction.Recipe{}, err
	}
	finalized, err := s.LoadFinalizedProduction(ctx, job.SetID, job.Revision)
	if err != nil || finalized.Authority.Receipt == nil ||
		finalized.Authority.Prepared.SHA256 != job.RevisionSHA256 ||
		finalized.Authority.Receipt.SHA256 != job.PreparedInputSHA256 {
		return redaction.Recipe{}, errors.Join(production.ErrJobConflict, err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return redaction.Recipe{}, err
	}
	defer func() { _ = tx.Rollback() }()
	recipe, err := loadProductionRecipeTx(ctx, tx, finalized.Draft)
	if err != nil {
		return redaction.Recipe{}, err
	}
	raw, err := canonical.Marshal(recipe)
	if err != nil {
		return redaction.Recipe{}, err
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != finalized.Authority.Prepared.RecipeSHA256 {
		return redaction.Recipe{}, production.ErrJobConflict
	}
	if err := tx.Commit(); err != nil {
		return redaction.Recipe{}, err
	}
	return recipe, nil
}

var _ production.LifecycleStore = (*Store)(nil)
