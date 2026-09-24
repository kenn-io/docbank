package store

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	productionservice "go.kenn.io/docbank/internal/production"
)

type productionJobCancelRequestV1 struct {
	OperationID string `json:"operation_id"`
	JobID       string `json:"job_id"`
	ETag        int64  `json:"etag"`
}

// CancelProductionJobOperation records a replay-safe cancellation against a
// job's pinned finalized-revision ETag. It never accepts a published job.
func (s *Store) CancelProductionJobOperation(ctx context.Context, actor, setID, jobID string, etag int64, operationID string) (redaction.Receipt, error) {
	if !validProductionActor(actor) || validateUUIDv4(setID) != nil || validateUUIDv4(jobID) != nil ||
		validateUUIDv4(operationID) != nil || etag < 1 {
		return redaction.Receipt{}, ErrInvalidProduction
	}
	var receipt redaction.Receipt
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var storedSetID, state string
		var revision, storedETag int64
		if err := tx.QueryRowContext(ctx, `SELECT set_id,revision,etag,state FROM production_jobs WHERE job_id=?`, jobID).
			Scan(&storedSetID, &revision, &storedETag, &state); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if storedSetID != setID {
			return ErrNotFound
		}
		request, err := canonicalProductionOperationRequest("job_cancel", setID, revision,
			productionJobCancelRequestV1{OperationID: operationID, JobID: jobID, ETag: etag})
		if err != nil || len(request) > 4096 {
			return errors.Join(ErrInvalidProduction, err)
		}
		requestSHA256 := productionSHA256(request)
		stored, found, err := loadProductionCancelOperationTx(ctx, tx, operationID, setID, requestSHA256)
		if err != nil {
			return err
		}
		if found {
			if state != productionservice.ProductionJobCanceled {
				return ErrInvalidProduction
			}
			receipt = stored
			return nil
		}
		if etag != storedETag {
			return ErrProductionRevisionConflict
		}
		if state == productionservice.ProductionJobSucceeded {
			return productionservice.ErrJobConflict
		}
		switch state {
		case productionservice.ProductionJobQueued, productionservice.ProductionJobRunning,
			productionservice.ProductionJobFailed, productionservice.ProductionJobCanceled:
		default:
			return ErrInvalidProduction
		}
		if _, err := tx.ExecContext(ctx, `UPDATE production_jobs SET state=?,cancel_requested=1,updated_at=? WHERE job_id=?`,
			productionservice.ProductionJobCanceled, nowRFC3339(), jobID); err != nil {
			return err
		}
		receipt = redaction.Receipt{OperationID: operationID, SetID: setID, Revision: revision,
			ETag: etag, RequestSHA256: requestSHA256}
		return recordProductionMutationTx(ctx, tx, actor, "job_cancel", receipt, nil)
	})
	return receipt, err
}

func loadProductionCancelOperationTx(ctx context.Context, tx *sql.Tx, operationID, setID, requestSHA256 string) (redaction.Receipt, bool, error) {
	var storedSetID, actor, kind, storedSHA256 string
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT set_id,actor,kind,request_sha256,receipt_json FROM production_operations WHERE operation_id=?`,
		operationID).Scan(&storedSetID, &actor, &kind, &storedSHA256, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return redaction.Receipt{}, false, nil
	}
	if err != nil {
		return redaction.Receipt{}, false, err
	}
	if storedSetID != setID || kind != "job_cancel" || storedSHA256 != requestSHA256 {
		return redaction.Receipt{}, false, ErrProductionOperationConflict
	}
	value, err := canonical.Decode[productionMutationReceiptV1](raw)
	if err != nil || value.Version != 1 || value.Kind != kind || value.Draft != nil ||
		redaction.ValidateReceipt(value.Receipt) != nil || value.Receipt.OperationID != operationID ||
		value.Receipt.SetID != setID || value.Receipt.RequestSHA256 != requestSHA256 {
		return redaction.Receipt{}, false, ErrInvalidProduction
	}
	if err := validateProductionOperationAuditTx(ctx, tx, operationID, setID, value.Receipt.Revision,
		actor, kind, requestSHA256, raw); err != nil {
		return redaction.Receipt{}, false, err
	}
	return value.Receipt, true, nil
}
