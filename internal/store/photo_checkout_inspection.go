package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"time"
)

// PhotoCheckoutInspection keeps a requested root local to the durable job.
type PhotoCheckoutInspection struct {
	CheckoutID  string `json:"checkout_id"`
	Destination string `json:"destination"`
	Identity    string `json:"identity"`
}

// PreparePhotoCheckoutOperation admits work after the caller's folder checks.
func (s *Store) PreparePhotoCheckoutOperation(ctx context.Context, request PhotoCheckoutInspection, kind string) (PhotoCheckout, StorageOperation, error) {
	var c PhotoCheckout
	var op StorageOperation
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var err error
		c, err = scanPhotoCheckout(tx.QueryRowContext(ctx, photoCheckoutSelect+` WHERE c.checkout_id=?`, request.CheckoutID))
		if err != nil {
			return err
		}
		switch kind {
		case StorageOperationKindPhotoCheckoutRebind:
			err = ValidatePhotoCheckoutRebind(c, "")
		case StorageOperationKindPhotoCheckoutScan, StorageOperationKindPhotoCheckoutCommit:
			err = ValidatePhotoCheckoutScanOrCommit(c, kind)
		default:
			return ErrInvalidPhotoCheckout
		}
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(request)
		if err != nil {
			return err
		}
		var total int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_checkout_entries WHERE checkout_id=?`, c.ID).Scan(&total); err != nil {
			return err
		}
		create := localOperationCreate(kind, string(encoded))
		create.TotalObjects = total
		op, err = createStorageOperationTx(ctx, tx, create)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE photo_checkouts SET last_operation_id=? WHERE checkout_id=?`, op.ID, c.ID)
		if err != nil {
			return err
		}
		c.LastOperationID, c.OperationKind, c.OperationState = op.ID, kind, string(StorageOperationQueued)
		return err
	})
	return c, op, err
}

// ReportPhotoCheckoutProgress preserves publication counters while hashing.
func (s *Store) ReportPhotoCheckoutProgress(ctx context.Context, id string, files int64, receipt string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE storage_operations SET completed_objects=?,receipt_json=?,updated_at=? WHERE operation_id=? AND state='running'`, files, receipt, nowRFC3339(), id)
	return requireOneStorageOperationRow(result, err, id, "reporting checkout verification")
}

func (s *Store) FinishPhotoCheckoutOperation(ctx context.Context, c PhotoCheckout, state StorageOperationState, receipt, failure string) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var progress struct {
			Files int64 `json:"files"`
		}
		if err := json.Unmarshal([]byte(receipt), &progress); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE storage_operations SET completed_objects=CASE WHEN ? THEN total_objects ELSE ? END WHERE operation_id=?`, state == StorageOperationCompleted, progress.Files, c.LastOperationID); err != nil {
			return err
		}
		if err := finishStorageOperationTx(ctx, tx, c.LastOperationID, state, receipt, failure, time.Now().Add(30*24*time.Hour)); err != nil {
			return err
		}
		return retireRequestedCheckoutTx(ctx, tx, c.ID)
	})
}
