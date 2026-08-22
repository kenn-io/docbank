package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/kit/packstore"
)

// StorageRecoveryPlan binds one explicit repair or fenced-store salvage to
// immutable content identity and the exact physical candidates reviewed.
type StorageRecoveryPlan struct {
	Version     int                      `json:"version"`
	Kind        string                   `json:"kind"`
	Digest      string                   `json:"digest"`
	Hash        string                   `json:"hash"`
	Size        int64                    `json:"size"`
	Sources     []packstore.ReadLocation `json:"sources"`
	Destination string                   `json:"destination"`
	Prior       *packstore.ReadLocation  `json:"prior,omitempty"`
}

func (s *Store) PlanStorageRecovery(
	ctx context.Context, kind, hash, storeSelector string,
) (StorageRecoveryPlan, error) {
	if kind != "repair" && kind != "salvage" {
		return StorageRecoveryPlan{}, errors.New("storage recovery must be repair or salvage")
	}
	parsed, err := packstore.ParseHash(hash)
	if err != nil {
		return StorageRecoveryPlan{}, fmt.Errorf("parsing recovery blob hash: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return StorageRecoveryPlan{}, fmt.Errorf("pinning storage recovery: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	selectedStore, err := blobStoreBySelectorTx(ctx, tx, storeSelector)
	if err != nil {
		return StorageRecoveryPlan{}, err
	}
	destination := selectedStore
	var requiredSource string
	if kind == "salvage" {
		if selectedStore.Role == blobStoreRolePrimary ||
			selectedStore.Lifecycle == blobStoreLifecycleDetached {
			return StorageRecoveryPlan{}, fmt.Errorf(
				"salvage source must be an attached secondary: %w",
				ErrBlobStoreState,
			)
		}
		destination, err = primaryBlobStoreTx(tx)
		if err != nil {
			return StorageRecoveryPlan{}, fmt.Errorf(
				"reading primary salvage destination: %w", err,
			)
		}
		requiredSource = selectedStore.ID
	}
	if destination.Lifecycle != blobStoreLifecycleActive {
		return StorageRecoveryPlan{}, fmt.Errorf("destination blob store %s is %s: %w",
			destination.ID, destination.Lifecycle, ErrBlobStoreState)
	}
	var size int64
	if err := tx.QueryRowContext(ctx,
		`SELECT size FROM blobs WHERE hash=?`, hash,
	).Scan(&size); errors.Is(err, sql.ErrNoRows) {
		return StorageRecoveryPlan{}, ErrNotFound
	} else if err != nil {
		return StorageRecoveryPlan{}, fmt.Errorf("reading recovery blob %s: %w", hash, err)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT b.size,l.store_id,l.generation,l.kind,l.encoding,l.stored_size,l.pack_eligible,
		       e.pack_id,e.pack_offset,e.stored_len,e.raw_len,e.flags,e.crc32c
		FROM blobs b JOIN blob_locations l ON l.blob_hash=b.hash
		LEFT JOIN blob_pack_entries e
		  ON e.blob_hash=l.blob_hash AND e.store_id=l.store_id
		WHERE b.hash=?
		ORDER BY CASE l.store_id WHEN ? THEN 0 ELSE 1 END,l.store_id`,
		hash, s.primaryStoreID,
	)
	if err != nil {
		return StorageRecoveryPlan{}, fmt.Errorf("reading recovery locations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var sources []packstore.ReadLocation
	var prior *packstore.ReadLocation
	for rows.Next() {
		location, present, err := scanBlobReadLocation(rows, parsed)
		if err != nil {
			return StorageRecoveryPlan{}, err
		}
		if !present {
			continue
		}
		if string(location.StoreID) == destination.ID {
			priorLocation := location
			prior = &priorLocation
			continue
		}
		if requiredSource == "" || string(location.StoreID) == requiredSource {
			sources = append(sources, location)
		}
	}
	if err := rows.Err(); err != nil {
		return StorageRecoveryPlan{}, fmt.Errorf("reading recovery locations: %w", err)
	}
	if len(sources) == 0 {
		return StorageRecoveryPlan{}, packstore.ErrPhysicalAuthorityMissing
	}
	if kind == "repair" && prior == nil {
		return StorageRecoveryPlan{}, fmt.Errorf(
			"repair destination has no authorized physical location: %w",
			packstore.ErrPhysicalAuthorityMissing,
		)
	}
	plan := StorageRecoveryPlan{
		Version: 1, Kind: kind, Hash: hash, Size: size,
		Sources: sources, Destination: destination.ID, Prior: prior,
	}
	digest, err := storageRecoveryDigest(plan)
	if err != nil {
		return StorageRecoveryPlan{}, err
	}
	plan.Digest = digest
	if err := tx.Commit(); err != nil {
		return StorageRecoveryPlan{}, fmt.Errorf("committing storage recovery preview: %w", err)
	}
	return plan, nil
}

func ValidateStorageRecoveryPlan(plan StorageRecoveryPlan) error {
	if plan.Version != 1 || (plan.Kind != "repair" && plan.Kind != "salvage") ||
		plan.Digest == "" || plan.Destination == "" || plan.Size < 0 {
		return errors.New("storage recovery plan is invalid")
	}
	if _, err := packstore.ParseHash(plan.Hash); err != nil {
		return fmt.Errorf("parsing storage recovery plan hash: %w", err)
	}
	if len(plan.Sources) == 0 {
		return errors.New("storage recovery plan has no sources")
	}
	seenSources := make(map[packstore.StoreID]bool, len(plan.Sources))
	for _, source := range plan.Sources {
		if err := source.Validate(); err != nil {
			return fmt.Errorf("validating storage recovery source: %w", err)
		}
		if source.StoreID == packstore.StoreID(plan.Destination) || seenSources[source.StoreID] {
			return errors.New("storage recovery plan has invalid source stores")
		}
		seenSources[source.StoreID] = true
	}
	if plan.Kind == "salvage" && len(plan.Sources) != 1 {
		return errors.New("salvage plan must name exactly one fenced source")
	}
	if plan.Kind == "repair" && plan.Prior == nil {
		return errors.New("repair plan lacks prior destination authority")
	}
	want, err := storageRecoveryDigest(plan)
	if err != nil {
		return err
	}
	if want != plan.Digest {
		return errors.New("storage recovery plan digest does not match its contents")
	}
	return nil
}

func storageRecoveryDigest(plan StorageRecoveryPlan) (string, error) {
	plan.Digest = ""
	data, err := json.Marshal(plan)
	if err != nil {
		return "", fmt.Errorf("encoding storage recovery plan: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

// BeginStorageRecoveryPublication rejects a stale preview and makes the
// operation non-cancellable before physical replacement. The runner holds the
// destination location lock across this transition and catalog publication.
func (s *Store) BeginStorageRecoveryPublication(
	ctx context.Context, operationID string, plan StorageRecoveryPlan,
) error {
	if err := ValidateStorageRecoveryPlan(plan); err != nil {
		return err
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var kind, digest, state, cursor string
		var cancelRequested bool
		if err := tx.QueryRowContext(ctx, `
			SELECT kind,request_digest,state,cursor,cancel_requested
			FROM storage_operations WHERE operation_id=?`,
			operationID,
		).Scan(&kind, &digest, &state, &cursor, &cancelRequested); err != nil {
			return fmt.Errorf("reading storage recovery operation: %w", err)
		}
		if kind != plan.Kind || digest != plan.Digest {
			return fmt.Errorf(
				"storage recovery operation does not bind this plan: %w",
				ErrStaleRevision,
			)
		}
		if StorageOperationState(state) != StorageOperationRunning {
			return fmt.Errorf(
				"storage operation %s is not publishable: %w",
				operationID, ErrStorageOperationTerminal,
			)
		}
		if err := validateStorageRecoveryPriorTx(tx, plan); err != nil {
			return err
		}
		if cursor == storageOperationFinalizingCursor {
			return nil
		}
		if cancelRequested {
			return ErrStorageOperationCancelled
		}
		return markStorageOperationFinalizingTx(ctx, tx, operationID)
	})
}

// CommitStorageRecovery grants the fully verified replacement generation.
func (s *Store) CommitStorageRecovery(
	ctx context.Context, operationID string,
	plan StorageRecoveryPlan, receipt packstore.ReadLocation,
	operationReceiptJSON string, retentionUntil time.Time,
) error {
	if err := validateUUIDv4(operationID); err != nil {
		return fmt.Errorf("invalid storage operation ID: %w", err)
	}
	if err := ValidateStorageRecoveryPlan(plan); err != nil {
		return err
	}
	if receipt.StoreID != packstore.StoreID(plan.Destination) ||
		receipt.Loose == nil || receipt.Pack != nil {
		return errors.New("storage recovery receipt does not name one destination loose object")
	}
	if err := receipt.Validate(); err != nil {
		return fmt.Errorf("validating storage recovery receipt: %w", err)
	}
	if operationReceiptJSON == "" {
		return errors.New("storage recovery operation receipt is empty")
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var kind, digest, state, cursor string
		var cancelRequested bool
		if err := tx.QueryRowContext(ctx, `
			SELECT kind,request_digest,state,cursor,cancel_requested
			FROM storage_operations WHERE operation_id=?`,
			operationID,
		).Scan(&kind, &digest, &state, &cursor, &cancelRequested); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("storage operation %s: %w", operationID, ErrNotFound)
			}
			return fmt.Errorf("reading storage operation before recovery commit: %w", err)
		}
		if kind != plan.Kind || digest != plan.Digest {
			return fmt.Errorf(
				"storage recovery operation does not bind this plan: %w",
				ErrStaleRevision,
			)
		}
		if cancelRequested && cursor != storageOperationFinalizingCursor {
			return ErrStorageOperationCancelled
		}
		if StorageOperationState(state) != StorageOperationRunning ||
			cursor != storageOperationFinalizingCursor {
			return fmt.Errorf(
				"storage operation %s is %s: %w",
				operationID, state, ErrStorageOperationTerminal,
			)
		}
		var size int64
		if err := tx.QueryRowContext(ctx,
			`SELECT size FROM blobs WHERE hash=?`, plan.Hash,
		).Scan(&size); err != nil {
			return fmt.Errorf("rechecking storage recovery membership: %w", err)
		}
		if size != plan.Size || receipt.Loose.LogicalSize != plan.Size {
			return packstore.ErrPhysicalCorrupt
		}
		if err := validateStorageRecoveryPriorTx(tx, plan); err != nil {
			return err
		}
		encoding, err := looseEncodingName(receipt.Loose.Encoding)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO blob_locations(
				blob_hash,store_id,generation,kind,encoding,stored_size,pack_eligible
			) VALUES(?,?,?,?,?,?,?)
			ON CONFLICT(blob_hash,store_id) DO UPDATE SET
				generation=excluded.generation,kind=excluded.kind,
				encoding=excluded.encoding,stored_size=excluded.stored_size,
				pack_eligible=excluded.pack_eligible`,
			plan.Hash, plan.Destination, receipt.Generation,
			blobLocationKindLoose, encoding, receipt.Loose.StoredSize,
			plan.Size <= maxPackEligibleBytes,
		)
		if err != nil {
			return fmt.Errorf("authorizing recovered location %s: %w", plan.Hash, err)
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM blob_pack_entries
			WHERE blob_hash=? AND store_id=?`,
			plan.Hash, plan.Destination,
		); err != nil {
			return fmt.Errorf("retiring damaged packed mapping %s: %w", plan.Hash, err)
		}
		now := nowRFC3339()
		result, err := tx.ExecContext(ctx, `
			UPDATE storage_operations
			SET state=?,cursor=?,completed_objects=1,copied_objects=1,copied_bytes=?,
			    receipt_json=?,updated_at=?,finished_at=?,retention_until=?
			WHERE operation_id=? AND state=? AND cursor=?`,
			StorageOperationCompleted, plan.Hash, plan.Size,
			operationReceiptJSON, now, now,
			retentionUntil.UTC().Format(timestampLayout),
			operationID, StorageOperationRunning, storageOperationFinalizingCursor,
		)
		return requireOneStorageOperationRow(
			result, err, operationID, "completing recovery",
		)
	})
}

func validateStorageRecoveryPriorTx(tx *sql.Tx, plan StorageRecoveryPlan) error {
	hash, err := packstore.ParseHash(plan.Hash)
	if err != nil {
		return fmt.Errorf("parsing recovery target hash: %w", err)
	}
	current, present, err := blobReadLocationTx(tx, hash, plan.Destination)
	if err != nil {
		return err
	}
	if plan.Prior == nil {
		if present {
			return fmt.Errorf("storage recovery destination changed: %w", ErrStaleRevision)
		}
		return nil
	}
	if !present || !sameReadLocation(current, *plan.Prior) {
		return fmt.Errorf("storage recovery destination changed: %w", ErrStaleRevision)
	}
	return nil
}

func blobReadLocationTx(
	tx *sql.Tx, hash packstore.Hash, storeID string,
) (packstore.ReadLocation, bool, error) {
	location, present, err := scanBlobReadLocation(tx.QueryRow(`
		SELECT b.size,l.store_id,l.generation,l.kind,l.encoding,
		       l.stored_size,l.pack_eligible,
		       e.pack_id,e.pack_offset,e.stored_len,e.raw_len,e.flags,e.crc32c
		FROM blobs b
		LEFT JOIN blob_locations l
		  ON l.blob_hash=b.hash AND l.store_id=?
		LEFT JOIN blob_pack_entries e
		  ON e.blob_hash=l.blob_hash AND e.store_id=l.store_id
		WHERE b.hash=?`,
		storeID, hash.String(),
	), hash)
	if errors.Is(err, sql.ErrNoRows) {
		return packstore.ReadLocation{}, false, nil
	}
	if err != nil {
		return packstore.ReadLocation{}, false, err
	}
	return location, present, nil
}
