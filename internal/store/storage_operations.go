package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"
	"unicode/utf8"

	"go.kenn.io/kit/packstore"
)

type StorageOperationState string

var ErrStorageOperationCancelled = errors.New("storage operation cancellation was requested")

const storageOperationFinalizingCursor = "@finalizing"

func (s *Store) boundPhotoCheckoutRebindSQL() string {
	d := metadataJSONDialect{postgres: s.postgresSchema != ""}
	return `kind='` + StorageOperationKindPhotoCheckoutRebind + `' AND EXISTS (
 SELECT 1 FROM photo_checkouts c WHERE c.last_operation_id=storage_operations.operation_id
 AND c.destination=` + d.text("storage_operations.request_json", "destination") + `
 AND c.destination_identity=` + d.text("storage_operations.request_json", "identity") + ` AND c.destination_reserved=1)`
}

const (
	storageOperationKindPlace       = "place"
	storageOperationKindEvacuate    = "evacuate"
	StorageOperationKindPhotoImport = "photo_import"
	storageOperationRoleSource      = "source"
	storageOperationRoleDestination = "destination"

	StorageOperationQueued    StorageOperationState = "queued"
	StorageOperationRunning   StorageOperationState = "running"
	StorageOperationCompleted StorageOperationState = "completed"
	StorageOperationFailed    StorageOperationState = "failed"
	StorageOperationCancelled StorageOperationState = "cancelled"
)

var storageOperationKinds = []string{
	storageOperationKindPlace, storageOperationKindEvacuate, "repair", "salvage", StorageOperationKindPhotoImport, StorageOperationKindPhotoCheckout, StorageOperationKindPhotoCheckoutCommit, StorageOperationKindPhotoCheckoutScan, StorageOperationKindPhotoCheckoutRebind,
}

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
	var out StorageOperation
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = createStorageOperationTx(ctx, tx, input)
		return err
	})
	return out, err
}

func createStorageOperationTx(ctx context.Context, tx *sql.Tx, input StorageOperationCreate) (StorageOperation, error) {
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
	err = func() error {
		if _, pruneErr := pruneExpiredStorageOperationsTx(
			ctx, tx, time.Now().UTC(),
		); pruneErr != nil {
			return pruneErr
		}
		if input.Kind == storageOperationKindEvacuate {
			var active int
			if err := tx.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM storage_operations
				WHERE source_store_id=$1 AND kind='evacuate'
				  AND state IN ($2,$3)`,
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
				(reference.Role == storageOperationRoleSource &&
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
			) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
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
				VALUES($1,$2,$3)`,
				id, reference.StoreID, reference.Role,
			); err != nil {
				return fmt.Errorf(
					"recording storage operation %s %s store %s: %w",
					id, reference.Role, reference.StoreID, err,
				)
			}
		}
		created, err = scanStorageOperation(tx.QueryRowContext(
			ctx, storageOperationSelect+` WHERE operation_id=$1`, id,
		))
		return err
	}()
	return created, err
}

// CreateLocalOperation records a durable job that references no blob store.
// It lists, reports progress, cancels, and resumes like a storage operation.
func (s *Store) CreateLocalOperation(ctx context.Context, kind, requestJSON string) (StorageOperation, error) {
	return s.CreateStorageOperation(ctx, localOperationCreate(kind, requestJSON))
}

func localOperationCreate(kind, requestJSON string) StorageOperationCreate {
	digest := sha256.Sum256([]byte(requestJSON))
	return StorageOperationCreate{
		Kind: kind, RequestDigest: hex.EncodeToString(digest[:]),
		RequestJSON: requestJSON, PlanJSON: "{}",
	}
}

// FinalizeLocalOperation reports whether a cancel request already reached a
// running local operation. When none has, it marks the operation finalizing in
// the same transaction, so a later cancel request is refused instead of being
// accepted and then ignored.
func (s *Store) FinalizeLocalOperation(ctx context.Context, id string) (bool, error) {
	var cancelled bool
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT cancel_requested FROM storage_operations
			WHERE operation_id=$1 AND state=$2`, id, StorageOperationRunning).Scan(&cancelled)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("finalizing storage operation %s: %w", id, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("reading storage operation %s before finalizing: %w", id, err)
		}
		if cancelled {
			return nil
		}
		return markStorageOperationFinalizingTx(ctx, tx, id)
	})
	return cancelled, err
}

// SetStorageOperationTotal records a total that a running operation
// discovers after it starts.
func (s *Store) SetStorageOperationTotal(ctx context.Context, id string, total int64) error {
	if total < 0 {
		return errors.New("storage operation total must not be negative")
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
		UPDATE storage_operations SET total_objects=$1,updated_at=$2
		WHERE operation_id=$3 AND state=$4`,
			total, nowRFC3339(), id, StorageOperationRunning,
		)
		return requireOneStorageOperationRow(result, err, id, "sizing")
	})
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
		WHERE state IN ($1,$2,$3)
		  AND retention_until IS NOT NULL
		  AND retention_until<=$4
		  AND NOT EXISTS (
			SELECT 1 FROM photo_checkouts c JOIN photo_checkout_entries e ON e.checkout_id=c.checkout_id
			WHERE c.operation_id=storage_operations.operation_id AND c.retired_at IS NULL
			  AND e.file_id=storage_operations.cursor AND e.published=0
		  )
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
		WHERE reference.store_id=$1 AND operation.state IN ($2,$3)
		  AND operation.operation_id<>$4`,
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
	if !slices.Contains(storageOperationKinds, input.Kind) {
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
			StoreID: input.SourceStoreID, Role: storageOperationRoleSource,
		})
	}
	byStore := make(map[string]string, len(references))
	unique := references[:0]
	for _, reference := range references {
		if err := validateUUIDv4(reference.StoreID); err != nil {
			return nil, fmt.Errorf("storage operation store ID is invalid: %w", err)
		}
		if reference.Role != storageOperationRoleSource &&
			reference.Role != storageOperationRoleDestination {
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
	var claimed StorageOperation
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		now := nowRFC3339()
		result, err := tx.ExecContext(ctx, `
		UPDATE storage_operations SET state=$1,cursor=CASE WHEN cursor=$2 AND kind IN ($3,$4,$5) AND NOT (`+s.boundPhotoCheckoutRebindSQL()+`) THEN '' ELSE cursor END,error='',updated_at=$6
		WHERE operation_id=$7 AND state IN ($8,$9)`,
			StorageOperationRunning, storageOperationFinalizingCursor, StorageOperationKindPhotoCheckoutScan, StorageOperationKindPhotoCheckoutCommit, StorageOperationKindPhotoCheckoutRebind, now, id,
			StorageOperationQueued, StorageOperationRunning,
		)
		if err != nil {
			return fmt.Errorf("claiming storage operation %s: %w", id, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("reading storage operation claim: %w", err)
		}
		if affected != 1 {
			return fmt.Errorf("storage operation %s is not resumable: %w", id, ErrStaleRevision)
		}
		claimed, err = scanStorageOperation(tx.QueryRowContext(ctx, storageOperationSelect+` WHERE operation_id=$1`, id))
		return err
	})
	return claimed, err
}

// DeferStorageOperation records a retryable failure or a clean operator pause and returns the
// operation to the durable queue without presenting it as actively running.
func (s *Store) DeferStorageOperation(ctx context.Context, id string, failure error) error {
	message := ""
	if failure != nil {
		message = failure.Error()
	}
	message = boundedStorageOperationError(message)
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
		UPDATE storage_operations SET state=$1,error=$2,updated_at=$3
		WHERE operation_id=$4 AND state=$5`,
			StorageOperationQueued, message, nowRFC3339(), id, StorageOperationRunning,
		)
		return requireOneStorageOperationRow(result, err, id, "deferring")
	})
}

// NoteQueuedStorageOperation records why a queued operation is waiting. A
// later claim clears it. An operation that left the queue is unchanged.
func (s *Store) NoteQueuedStorageOperation(ctx context.Context, id, message string) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
		UPDATE storage_operations SET error=$1,updated_at=$2
		WHERE operation_id=$3 AND state=$4`,
			boundedStorageOperationError(message), nowRFC3339(), id, StorageOperationQueued,
		); err != nil {
			return fmt.Errorf("noting queued storage operation %s: %w", id, err)
		}
		return nil
	})
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
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
		UPDATE storage_operations
		SET cursor=$1,completed_objects=$2,copied_objects=$3,copied_bytes=$4,
		    receipt_json=CASE WHEN $5='' THEN receipt_json ELSE $6 END,updated_at=$7
		WHERE operation_id=$8 AND state=$9`,
			cursor, completedObjects, copiedObjects, copiedBytes, receipt, receipt, nowRFC3339(),
			id, StorageOperationRunning,
		)
		return requireOneStorageOperationRow(result, err, id, "advancing")
	})
}

func (s *Store) RequestStorageOperationCancel(ctx context.Context, id string) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
		UPDATE storage_operations SET cancel_requested=1,updated_at=$1
		WHERE operation_id=$2 AND ((state=$3 AND (cursor<>$4 OR (kind IN ($5,$6,$7) AND NOT (`+s.boundPhotoCheckoutRebindSQL()+`)))) OR (state=$8 AND cursor<>$9))`,
			nowRFC3339(), id, StorageOperationQueued, storageOperationFinalizingCursor,
			StorageOperationKindPhotoCheckoutScan, StorageOperationKindPhotoCheckoutCommit, StorageOperationKindPhotoCheckoutRebind, StorageOperationRunning, storageOperationFinalizingCursor,
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
		err = tx.QueryRowContext(ctx,
			`SELECT state,cursor FROM storage_operations WHERE operation_id=$1`, id,
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
	})
}

func (s *Store) FinishStorageOperation(
	ctx context.Context, id string, state StorageOperationState,
	receiptJSON, failure string, retentionUntil time.Time,
) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		return finishStorageOperationTx(ctx, tx, id, state, receiptJSON, failure, retentionUntil)
	})
}

func finishStorageOperationTx(ctx context.Context, tx *sql.Tx, id string, state StorageOperationState, receiptJSON, failure string, retentionUntil time.Time) error {
	switch state {
	case StorageOperationCompleted, StorageOperationFailed, StorageOperationCancelled:
	default:
		return fmt.Errorf("storage operation terminal state %q is invalid", state)
	}
	failure = boundedStorageOperationError(failure)
	now := nowRFC3339()
	var retention any
	if !retentionUntil.IsZero() {
		retention = retentionUntil.UTC().Format(timestampLayout)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE storage_operations
		SET state=$1,receipt_json=$2,error=$3,updated_at=$4,finished_at=$5,retention_until=$6
		WHERE operation_id=$7 AND state IN ($8,$9)`,
		state, receiptJSON, failure, now, now, retention, id,
		StorageOperationQueued, StorageOperationRunning,
	)
	return requireOneStorageOperationRow(result, err, id, "finishing")
}

func boundedStorageOperationError(message string) string {
	if len(message) > 4096 {
		message = message[:4096]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	return message
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
		` WHERE operation_id=$1`, id))
}

func (s *Store) StorageOperations(
	ctx context.Context, limit int,
) ([]StorageOperation, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("storage operation limit must be between 1 and 1000")
	}
	rows, err := s.db.QueryContext(ctx, storageOperationSelect+
		` ORDER BY created_at DESC,operation_id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("listing storage operations: %w", err)
	}
	return scanStorageOperations(rows)
}

func (s *Store) ResumableStorageOperations(ctx context.Context) ([]StorageOperation, error) {
	rows, err := s.db.QueryContext(ctx, storageOperationSelect+
		` WHERE state IN ($1,$2) ORDER BY created_at,operation_id`,
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
			INSERT INTO storage_operation_cleanup(
				operation_id,store_id,loose_hash,loose_encoding,pack_id
			) VALUES($1,$2,$3,$4,$5) ON CONFLICT(operation_id,store_id,loose_hash,loose_encoding,pack_id) DO NOTHING`,
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
		WHERE operation_id=$1
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
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
		DELETE FROM storage_operation_cleanup
		WHERE operation_id=$1 AND store_id=$2 AND loose_hash=$3 AND loose_encoding=$4 AND pack_id=$5`,
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
	})
}

// PrepareStorageOperationCleanup determines whether a retired physical object
// is still safe to remove. If the exact object has become authoritative again,
// it atomically consumes the stale cleanup item and returns false.
func (s *Store) PrepareStorageOperationCleanup(
	ctx context.Context, operationID string, item StorageOperationCleanup,
) (bool, error) {
	var retire bool
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM storage_operation_cleanup
				WHERE operation_id=$1 AND store_id=$2 AND loose_hash=$3
				  AND loose_encoding=$4 AND pack_id=$5
			)`,
			operationID, item.StoreID, item.Ref.LooseHash.String(),
			item.Ref.LooseEncoding, item.Ref.PackID,
		).Scan(&exists); err != nil {
			return fmt.Errorf("checking storage operation cleanup: %w", err)
		}
		if !exists {
			return fmt.Errorf("storage cleanup no longer exists: %w", ErrNotFound)
		}

		var authorized bool
		if item.Ref.LooseHash != "" {
			encoding, err := looseEncodingName(item.Ref.LooseEncoding)
			if err != nil {
				return err
			}
			if err := tx.QueryRowContext(ctx, `
				SELECT EXISTS(
					SELECT 1 FROM blob_locations
					WHERE blob_hash=$1 AND store_id=$2 AND kind=$3 AND encoding=$4
				)`,
				item.Ref.LooseHash.String(), item.StoreID,
				blobLocationKindLoose, encoding,
			).Scan(&authorized); err != nil {
				return fmt.Errorf("checking loose cleanup authority: %w", err)
			}
		} else {
			if err := tx.QueryRowContext(ctx, `
				SELECT EXISTS(
					SELECT 1 FROM blob_packs WHERE store_id=$1 AND pack_id=$2
				)`, item.StoreID, item.Ref.PackID,
			).Scan(&authorized); err != nil {
				return fmt.Errorf("checking pack cleanup authority: %w", err)
			}
		}
		if authorized {
			result, err := tx.ExecContext(ctx, `
				DELETE FROM storage_operation_cleanup
				WHERE operation_id=$1 AND store_id=$2 AND loose_hash=$3
				  AND loose_encoding=$4 AND pack_id=$5`,
				operationID, item.StoreID, item.Ref.LooseHash.String(),
				item.Ref.LooseEncoding, item.Ref.PackID,
			)
			if err != nil {
				return fmt.Errorf("discarding reauthorized storage cleanup: %w", err)
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("reading reauthorized cleanup discard: %w", err)
			}
			if affected != 1 {
				return fmt.Errorf("storage cleanup changed during preparation: %w", ErrStaleRevision)
			}
			return nil
		}
		retire = true
		return nil
	})
	return retire, err
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
