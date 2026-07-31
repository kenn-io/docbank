package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

type StorageOperationState string

const (
	StorageOperationQueued    StorageOperationState = "queued"
	StorageOperationRunning   StorageOperationState = "running"
	StorageOperationCompleted StorageOperationState = "completed"
	StorageOperationFailed    StorageOperationState = "failed"
	StorageOperationCancelled StorageOperationState = "cancelled"
)

type StorageOperationCreate struct {
	Kind          string
	RequestDigest string
	RequestJSON   string
	PlanJSON      string
	TotalObjects  int64
}

type StorageOperation struct {
	ID               string
	Kind             string
	RequestVersion   int64
	RequestDigest    string
	RequestJSON      string
	PlanJSON         string
	State            StorageOperationState
	Cursor           string
	TotalObjects     int64
	CompletedObjects int64
	CopiedObjects    int64
	CopiedBytes      int64
	CancelRequested  bool
	Error            string
	ReceiptJSON      string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	FinishedAt       *time.Time
	RetentionUntil   *time.Time
}

func (s *Store) CreateStorageOperation(
	ctx context.Context, input StorageOperationCreate,
) (StorageOperation, error) {
	if err := validateStorageOperationCreate(input); err != nil {
		return StorageOperation{}, err
	}
	id, err := newUUIDv4()
	if err != nil {
		return StorageOperation{}, fmt.Errorf("creating storage operation identity: %w", err)
	}
	now := nowRFC3339()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO storage_operations(
			operation_id,kind,request_version,request_digest,request_json,plan_json,
			state,total_objects,created_at,updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		id, input.Kind, 1, input.RequestDigest, input.RequestJSON, input.PlanJSON,
		StorageOperationQueued, input.TotalObjects, now, now,
	)
	if err != nil {
		return StorageOperation{}, fmt.Errorf("creating storage operation: %w", err)
	}
	return s.StorageOperation(ctx, id)
}

func validateStorageOperationCreate(input StorageOperationCreate) error {
	switch input.Kind {
	case "place", "evacuate", "repair", "salvage":
	default:
		return fmt.Errorf("unsupported storage operation kind %q", input.Kind)
	}
	if len(input.RequestDigest) != 64 {
		return errors.New("storage operation request digest must be a SHA-256 identity")
	}
	if input.TotalObjects < 0 {
		return errors.New("storage operation total must not be negative")
	}
	if !utf8.ValidString(input.RequestJSON) || !utf8.ValidString(input.PlanJSON) ||
		input.RequestJSON == "" || input.PlanJSON == "" {
		return errors.New("storage operation request and plan must be nonempty UTF-8 JSON")
	}
	return nil
}

func (s *Store) ClaimStorageOperation(
	ctx context.Context, id string,
) (StorageOperation, error) {
	if err := validateUUIDv4(id); err != nil {
		return StorageOperation{}, fmt.Errorf("invalid storage operation ID: %w", err)
	}
	now := nowRFC3339()
	result, err := s.db.ExecContext(ctx, `
		UPDATE storage_operations SET state=?,updated_at=?
		WHERE operation_id=? AND state IN (?,?)`,
		StorageOperationRunning, now, id,
		StorageOperationQueued, StorageOperationRunning,
	)
	if err != nil {
		return StorageOperation{}, fmt.Errorf("claiming storage operation %s: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return StorageOperation{}, fmt.Errorf("reading storage operation claim: %w", err)
	}
	if affected != 1 {
		return StorageOperation{}, fmt.Errorf("storage operation %s is not resumable: %w", id, ErrStaleRevision)
	}
	return s.StorageOperation(ctx, id)
}

func (s *Store) AdvanceStorageOperation(
	ctx context.Context, id, cursor string,
	completedObjects, copiedObjects, copiedBytes int64,
	progressJSON ...string,
) error {
	if completedObjects < 0 || copiedObjects < 0 || copiedBytes < 0 ||
		copiedObjects > completedObjects {
		return errors.New("invalid storage operation progress")
	}
	receipt := ""
	if len(progressJSON) > 1 {
		return errors.New("storage operation accepts at most one progress receipt")
	}
	if len(progressJSON) == 1 {
		receipt = progressJSON[0]
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE storage_operations
		SET cursor=?,completed_objects=?,copied_objects=?,copied_bytes=?,
		    receipt_json=CASE WHEN ?='' THEN receipt_json ELSE ? END,updated_at=?
		WHERE operation_id=? AND state=?`,
		cursor, completedObjects, copiedObjects, copiedBytes, receipt, receipt, nowRFC3339(),
		id, StorageOperationRunning,
	)
	return requireOneStorageOperationRow(result, err, id, "advancing")
}

func (s *Store) RequestStorageOperationCancel(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE storage_operations SET cancel_requested=1,updated_at=?
		WHERE operation_id=? AND state IN (?,?)`,
		nowRFC3339(), id, StorageOperationQueued, StorageOperationRunning,
	)
	if err != nil {
		return fmt.Errorf("requesting cancellation for storage operation %s: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("reading cancellation result for storage operation %s: %w", id, err)
	}
	if affected == 1 {
		return nil
	}
	var state string
	err = s.db.QueryRowContext(ctx,
		`SELECT state FROM storage_operations WHERE operation_id=?`, id,
	).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("storage operation %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("reading storage operation %s after cancellation: %w", id, err)
	}
	return fmt.Errorf("storage operation %s is %s: %w",
		id, state, ErrStorageOperationTerminal)
}

func (s *Store) FinishStorageOperation(
	ctx context.Context, id string, state StorageOperationState,
	receiptJSON, failure string, retentionUntil time.Time,
) error {
	switch state {
	case StorageOperationCompleted, StorageOperationFailed, StorageOperationCancelled:
	default:
		return fmt.Errorf("storage operation terminal state %q is invalid", state)
	}
	if len(failure) > 4096 {
		failure = failure[:4096]
		for !utf8.ValidString(failure) {
			failure = failure[:len(failure)-1]
		}
	}
	now := nowRFC3339()
	var retention any
	if !retentionUntil.IsZero() {
		retention = retentionUntil.UTC().Format(timestampLayout)
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE storage_operations
		SET state=?,receipt_json=?,error=?,updated_at=?,finished_at=?,retention_until=?
		WHERE operation_id=? AND state IN (?,?)`,
		state, receiptJSON, failure, now, now, retention, id,
		StorageOperationQueued, StorageOperationRunning,
	)
	return requireOneStorageOperationRow(result, err, id, "finishing")
}

func requireOneStorageOperationRow(
	result sql.Result, err error, id, action string,
) error {
	if err != nil {
		return fmt.Errorf("%s storage operation %s: %w", action, id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("reading %s storage operation %s result: %w", action, id, err)
	}
	if affected != 1 {
		return fmt.Errorf("%s storage operation %s: %w", action, id, ErrNotFound)
	}
	return nil
}

func (s *Store) StorageOperation(ctx context.Context, id string) (StorageOperation, error) {
	return scanStorageOperation(s.db.QueryRowContext(ctx, storageOperationSelect+
		` WHERE operation_id=?`, id))
}

func (s *Store) StorageOperations(
	ctx context.Context, limit int,
) ([]StorageOperation, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("storage operation limit must be between 1 and 1000")
	}
	rows, err := s.db.QueryContext(ctx, storageOperationSelect+
		` ORDER BY created_at DESC,operation_id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("listing storage operations: %w", err)
	}
	return scanStorageOperations(rows)
}

func (s *Store) ResumableStorageOperations(ctx context.Context) ([]StorageOperation, error) {
	rows, err := s.db.QueryContext(ctx, storageOperationSelect+
		` WHERE state IN (?,?) ORDER BY created_at,operation_id`,
		StorageOperationQueued, StorageOperationRunning)
	if err != nil {
		return nil, fmt.Errorf("listing resumable storage operations: %w", err)
	}
	return scanStorageOperations(rows)
}

const storageOperationSelect = `
	SELECT operation_id,kind,request_version,request_digest,request_json,plan_json,
	       state,cursor,total_objects,completed_objects,copied_objects,copied_bytes,
	       cancel_requested,error,receipt_json,created_at,updated_at,finished_at,retention_until
	FROM storage_operations`

func scanStorageOperations(rows *sql.Rows) ([]StorageOperation, error) {
	defer func() { _ = rows.Close() }()
	var result []StorageOperation
	for rows.Next() {
		operation, err := scanStorageOperation(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, operation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing storage operations: %w", err)
	}
	return result, nil
}

func scanStorageOperation(row scanner) (StorageOperation, error) {
	var operation StorageOperation
	var state string
	var createdAt, updatedAt string
	var finishedAt, retentionUntil sql.NullString
	err := row.Scan(
		&operation.ID, &operation.Kind, &operation.RequestVersion,
		&operation.RequestDigest, &operation.RequestJSON, &operation.PlanJSON,
		&state, &operation.Cursor, &operation.TotalObjects,
		&operation.CompletedObjects, &operation.CopiedObjects, &operation.CopiedBytes,
		&operation.CancelRequested, &operation.Error, &operation.ReceiptJSON,
		&createdAt, &updatedAt, &finishedAt, &retentionUntil,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return StorageOperation{}, ErrNotFound
	}
	if err != nil {
		return StorageOperation{}, fmt.Errorf("reading storage operation: %w", err)
	}
	operation.State = StorageOperationState(state)
	operation.CreatedAt = parseStoredTime(createdAt)
	operation.UpdatedAt = parseStoredTime(updatedAt)
	if finishedAt.Valid {
		value := parseStoredTime(finishedAt.String)
		operation.FinishedAt = &value
	}
	if retentionUntil.Valid {
		value := parseStoredTime(retentionUntil.String)
		operation.RetentionUntil = &value
	}
	return operation, nil
}
