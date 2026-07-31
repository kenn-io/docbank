package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
	"unicode/utf8"

	"go.kenn.io/kit/packstore"
)

type StorageOperationState string

var ErrStorageOperationCancelled = errors.New("storage operation cancellation was requested")

const storageOperationFinalizingCursor = "@finalizing"

const (
	storageOperationKindEvacuate = "evacuate"

	StorageOperationQueued    StorageOperationState = "queued"
	StorageOperationRunning   StorageOperationState = "running"
	StorageOperationCompleted StorageOperationState = "completed"
	StorageOperationFailed    StorageOperationState = "failed"
	StorageOperationCancelled StorageOperationState = "cancelled"
)

type StorageOperationCreate struct {
	Kind            string
	SourceStoreID   string
	StoreReferences []StorageOperationStoreReference
	RequestDigest   string
	RequestJSON     string
	PlanJSON        string
	TotalObjects    int64
}

type StorageOperationStoreReference struct {
	StoreID string
	Role    string
}

type StorageOperation struct {
	ID               string
	Kind             string
	SourceStoreID    string
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

type StorageOperationCleanup struct {
	StoreID string
	Ref     packstore.ObjectRef
}

func (s *Store) CreateStorageOperation(
	ctx context.Context, input StorageOperationCreate,
) (StorageOperation, error) {
	storeReferences, err := validateStorageOperationCreate(input)
	if err != nil {
		return StorageOperation{}, err
	}
	id, err := newUUIDv4()
	if err != nil {
		return StorageOperation{}, fmt.Errorf("creating storage operation identity: %w", err)
	}
	now := nowRFC3339()
	sourceStoreID := sql.NullString{
		String: input.SourceStoreID,
		Valid:  input.SourceStoreID != "",
	}
	var created StorageOperation
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if _, pruneErr := pruneExpiredStorageOperationsTx(
			ctx, tx, time.Now().UTC(),
		); pruneErr != nil {
			return pruneErr
		}
		if input.Kind == storageOperationKindEvacuate {
			var active int
			if err := tx.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM storage_operations
				WHERE source_store_id=? AND kind='evacuate'
				  AND state IN (?,?)`,
				input.SourceStoreID, StorageOperationQueued, StorageOperationRunning,
			).Scan(&active); err != nil {
				return fmt.Errorf("checking active evacuation: %w", err)
			}
			if active != 0 {
				return fmt.Errorf(
					"blob store %s already has an active evacuation: %w",
					input.SourceStoreID, ErrBlobStoreState,
				)
			}
		}
		for _, reference := range storeReferences {
			store, err := blobStoreBySelectorTx(ctx, tx, reference.StoreID)
			if err != nil {
				return fmt.Errorf(
					"reading storage operation %s store %s: %w",
					reference.Role, reference.StoreID, err,
				)
			}
			allowed := store.Lifecycle == blobStoreLifecycleActive ||
				(reference.Role == "source" &&
					store.Lifecycle == blobStoreLifecycleDraining)
			if !allowed {
				return fmt.Errorf(
					"storage operation %s store %s is %s: %w",
					reference.Role, reference.StoreID, store.Lifecycle,
					ErrBlobStoreState,
				)
			}
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO storage_operations(
				operation_id,kind,source_store_id,request_version,request_digest,
				request_json,plan_json,state,total_objects,created_at,updated_at
			) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			id, input.Kind, sourceStoreID, 1, input.RequestDigest,
			input.RequestJSON, input.PlanJSON, StorageOperationQueued,
			input.TotalObjects, now, now,
		)
		if err != nil {
			return fmt.Errorf("creating storage operation: %w", err)
		}
		for _, reference := range storeReferences {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO storage_operation_stores(operation_id,store_id,role)
				VALUES(?,?,?)`,
				id, reference.StoreID, reference.Role,
			); err != nil {
				return fmt.Errorf(
					"recording storage operation %s %s store %s: %w",
					id, reference.Role, reference.StoreID, err,
				)
			}
		}
		created, err = scanStorageOperation(tx.QueryRowContext(
			ctx, storageOperationSelect+` WHERE operation_id=?`, id,
		))
		return err
	})
	return created, err
}

// PruneExpiredStorageOperations removes terminal operation receipts after
// their retention boundary while preserving every operation that still owns
// pending physical cleanup.
func (s *Store) PruneExpiredStorageOperations(
	ctx context.Context, now time.Time,
) (int64, error) {
	if now.IsZero() {
		return 0, errors.New("storage operation prune time is required")
	}
	var pruned int64
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var err error
		pruned, err = pruneExpiredStorageOperationsTx(ctx, tx, now.UTC())
		return err
	})
	return pruned, err
}

func pruneExpiredStorageOperationsTx(
	ctx context.Context, tx *sql.Tx, now time.Time,
) (int64, error) {
	result, err := tx.ExecContext(ctx, `
		DELETE FROM storage_operations
		WHERE state IN (?,?,?)
		  AND retention_until IS NOT NULL
		  AND retention_until<=?
		  AND NOT EXISTS (
			SELECT 1 FROM storage_operation_cleanup cleanup
			WHERE cleanup.operation_id=storage_operations.operation_id
		  )`,
		StorageOperationCompleted, StorageOperationFailed,
		StorageOperationCancelled, now.Format(timestampLayout),
	)
	if err != nil {
		return 0, fmt.Errorf("pruning expired storage operations: %w", err)
	}
	pruned, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("reading expired storage operation prune result: %w", err)
	}
	return pruned, nil
}

func activeStorageOperationsForStoreTx(
	ctx context.Context, tx *sql.Tx, storeID, excludedOperationID string,
) (int, error) {
	var active int
	err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM storage_operation_stores reference
		JOIN storage_operations operation
		  ON operation.operation_id=reference.operation_id
		WHERE reference.store_id=? AND operation.state IN (?,?)
		  AND operation.operation_id<>?`,
		storeID, StorageOperationQueued, StorageOperationRunning,
		excludedOperationID,
	).Scan(&active)
	if err != nil {
		return 0, fmt.Errorf("checking active storage operations: %w", err)
	}
	return active, nil
}

func validateStorageOperationCreate(
	input StorageOperationCreate,
) ([]StorageOperationStoreReference, error) {
	switch input.Kind {
	case "place", storageOperationKindEvacuate, "repair", "salvage":
	default:
		return nil, fmt.Errorf("unsupported storage operation kind %q", input.Kind)
	}
	if input.Kind == storageOperationKindEvacuate {
		if err := validateUUIDv4(input.SourceStoreID); err != nil {
			return nil, fmt.Errorf("evacuation source store ID is invalid: %w", err)
		}
	} else if input.SourceStoreID != "" {
		return nil, errors.New("only evacuation operations may bind a source store")
	}
	if len(input.RequestDigest) != 64 {
		return nil, errors.New("storage operation request digest must be a SHA-256 identity")
	}
	if input.TotalObjects < 0 {
		return nil, errors.New("storage operation total must not be negative")
	}
	if !utf8.ValidString(input.RequestJSON) || !utf8.ValidString(input.PlanJSON) ||
		input.RequestJSON == "" || input.PlanJSON == "" {
		return nil, errors.New("storage operation request and plan must be nonempty UTF-8 JSON")
	}
	references := append([]StorageOperationStoreReference(nil), input.StoreReferences...)
	if input.SourceStoreID != "" {
		references = append(references, StorageOperationStoreReference{
			StoreID: input.SourceStoreID, Role: "source",
		})
	}
	byStore := make(map[string]string, len(references))
	unique := references[:0]
	for _, reference := range references {
		if err := validateUUIDv4(reference.StoreID); err != nil {
			return nil, fmt.Errorf("storage operation store ID is invalid: %w", err)
		}
		if reference.Role != "source" && reference.Role != "destination" {
			return nil, fmt.Errorf("storage operation store role %q is invalid", reference.Role)
		}
		if prior, exists := byStore[reference.StoreID]; exists {
			if prior != reference.Role {
				return nil, fmt.Errorf(
					"storage operation store %s cannot be both %s and %s",
					reference.StoreID, prior, reference.Role,
				)
			}
			continue
		}
		byStore[reference.StoreID] = reference.Role
		unique = append(unique, reference)
	}
	sort.Slice(unique, func(i, j int) bool {
		return unique[i].StoreID < unique[j].StoreID
	})
	return unique, nil
}

func (s *Store) ClaimStorageOperation(
	ctx context.Context, id string,
) (StorageOperation, error) {
	if err := validateUUIDv4(id); err != nil {
		return StorageOperation{}, fmt.Errorf("invalid storage operation ID: %w", err)
	}
	now := nowRFC3339()
	result, err := s.db.ExecContext(ctx, `
		UPDATE storage_operations SET state=?,error='',updated_at=?
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

// DeferStorageOperation records a retryable worker failure and returns the
// operation to the durable queue without presenting it as actively running.
func (s *Store) DeferStorageOperation(ctx context.Context, id string, failure error) error {
	if failure == nil {
		return errors.New("deferred storage operation requires a failure")
	}
	message := failure.Error()
	if len(message) > 4096 {
		message = message[:4096]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE storage_operations SET state=?,error=?,updated_at=?
		WHERE operation_id=? AND state=?`,
		StorageOperationQueued, message, nowRFC3339(), id, StorageOperationRunning,
	)
	return requireOneStorageOperationRow(result, err, id, "deferring")
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
		WHERE operation_id=? AND state IN (?,?) AND cursor<>?`,
		nowRFC3339(), id, StorageOperationQueued, StorageOperationRunning,
		storageOperationFinalizingCursor,
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
	var state, cursor string
	err = s.db.QueryRowContext(ctx,
		`SELECT state,cursor FROM storage_operations WHERE operation_id=?`, id,
	).Scan(&state, &cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("storage operation %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("reading storage operation %s after cancellation: %w", id, err)
	}
	if StorageOperationState(state) == StorageOperationRunning &&
		cursor == storageOperationFinalizingCursor {
		return fmt.Errorf(
			"storage operation %s is finalizing and no longer cancellable: %w",
			id, ErrStorageOperationTerminal,
		)
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

func recordStorageOperationCleanupTx(
	ctx context.Context,
	tx *sql.Tx,
	operationID, storeID string,
	refs []packstore.ObjectRef,
) error {
	for _, ref := range refs {
		if (ref.LooseHash == "") == (ref.PackID == "") {
			return errors.New("storage cleanup must name exactly one loose object or pack")
		}
		if ref.LooseHash != "" {
			if err := ref.LooseHash.Validate(); err != nil {
				return fmt.Errorf("validating cleanup blob hash: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO storage_operation_cleanup(
				operation_id,store_id,loose_hash,loose_encoding,pack_id
			) VALUES(?,?,?,?,?)`,
			operationID, storeID, ref.LooseHash.String(), ref.LooseEncoding, ref.PackID,
		); err != nil {
			return fmt.Errorf("recording storage cleanup: %w", err)
		}
	}
	return nil
}

func (s *Store) StorageOperationCleanups(
	ctx context.Context, operationID string,
) ([]StorageOperationCleanup, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT store_id,loose_hash,loose_encoding,pack_id
		FROM storage_operation_cleanup
		WHERE operation_id=?
		ORDER BY store_id,pack_id,loose_hash,loose_encoding`,
		operationID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing storage operation cleanup: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var result []StorageOperationCleanup
	for rows.Next() {
		var item StorageOperationCleanup
		var hash, packID string
		var encoding int
		if err := rows.Scan(&item.StoreID, &hash, &encoding, &packID); err != nil {
			return nil, fmt.Errorf("scanning storage operation cleanup: %w", err)
		}
		if hash != "" {
			parsed, err := packstore.ParseHash(hash)
			if err != nil {
				return nil, fmt.Errorf("parsing storage cleanup blob hash: %w", err)
			}
			if encoding != int(packstore.LooseEncodingRaw) &&
				encoding != int(packstore.LooseEncodingZstd) {
				return nil, fmt.Errorf("invalid storage cleanup encoding %d", encoding)
			}
			item.Ref.LooseHash = parsed
			item.Ref.LooseEncoding = packstore.LooseEncoding(encoding)
		} else {
			item.Ref.PackID = packID
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing storage operation cleanup: %w", err)
	}
	return result, nil
}

func (s *Store) CompleteStorageOperationCleanup(
	ctx context.Context, operationID string, item StorageOperationCleanup,
) error {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM storage_operation_cleanup
		WHERE operation_id=? AND store_id=? AND loose_hash=? AND loose_encoding=? AND pack_id=?`,
		operationID, item.StoreID, item.Ref.LooseHash.String(),
		item.Ref.LooseEncoding, item.Ref.PackID,
	)
	if err != nil {
		return fmt.Errorf("completing storage operation cleanup: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("reading storage cleanup completion: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("storage cleanup no longer exists: %w", ErrNotFound)
	}
	return nil
}

const storageOperationSelect = `
	SELECT operation_id,kind,COALESCE(source_store_id,''),request_version,
	       request_digest,request_json,plan_json,
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
		&operation.ID, &operation.Kind, &operation.SourceStoreID, &operation.RequestVersion,
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
