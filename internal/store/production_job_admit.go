package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"go.kenn.io/docbank/internal/canonical"
	productionservice "go.kenn.io/docbank/internal/production"
)

// AdmitFinalizedProductionJob derives every digest from retained finalized
// authority and checks the current policy gate before inserting a new job.
// An exact retry returns the existing job without re-evaluating a later source
// or approval change; the worker still verifies current authority before use.
func (s *Store) AdmitFinalizedProductionJob(ctx context.Context, setID string, revision, etag int64,
	jobID, operationID string) (productionservice.Job, error) {
	if validateUUIDv4(setID) != nil || validateUUIDv4(jobID) != nil || validateUUIDv4(operationID) != nil ||
		revision < 1 || etag < 1 {
		return productionservice.Job{}, ErrInvalidProduction
	}
	var job productionservice.Job
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var existingRaw []byte
		err := tx.QueryRowContext(ctx, `SELECT request_json FROM production_jobs WHERE job_id=?`, jobID).Scan(&existingRaw)
		if err == nil {
			if len(existingRaw) == 0 || len(existingRaw) > maxProductionJobRequestBytes {
				return ErrInvalidProduction
			}
			request, err := canonical.Decode[productionservice.JobRequest](existingRaw)
			if err != nil || request.JobID != jobID || request.OperationID != operationID ||
				request.SetID != setID || request.Revision != revision {
				return productionservice.ErrJobConflict
			}
			if request.ETag != etag {
				return ErrProductionRevisionConflict
			}
			job, err = s.admitProductionJobTx(ctx, tx, request, existingRaw)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var occupiedJobID string
		err = tx.QueryRowContext(ctx, `SELECT job_id FROM production_jobs WHERE operation_id=?`, operationID).Scan(&occupiedJobID)
		if err == nil {
			return productionservice.ErrJobConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		finalized, err := s.loadFinalizedProductionTx(ctx, tx, setID, revision)
		if err != nil {
			return err
		}
		if finalized.Draft.ETag != etag {
			return ErrProductionRevisionConflict
		}
		current, err := s.LoadProductionGateSnapshot(ctx, tx, productionservice.PreparedInputRequest{
			SetID: setID, Revision: revision, PreparedAt: time.Now().UTC(),
		})
		if err != nil || current.Draft != finalized.Draft {
			return errors.Join(ErrInvalidProduction, err)
		}
		if finalized.Authority.Receipt == nil {
			return ErrInvalidProduction
		}
		request := productionservice.JobRequest{JobID: jobID, OperationID: operationID,
			SetID: setID, Revision: revision, ETag: etag,
			PreparedInputSHA256:    finalized.Authority.Receipt.SHA256,
			RevisionSHA256:         finalized.Authority.Prepared.SHA256,
			NumberingProfileSHA256: finalized.Draft.NumberingRecipeSHA256}
		raw, err := canonical.Marshal(request)
		if err != nil || len(raw) > maxProductionJobRequestBytes {
			return errors.Join(productionservice.ErrJobConflict, err)
		}
		job, err = s.admitProductionJobTx(ctx, tx, request, raw)
		return err
	})
	return job, err
}
