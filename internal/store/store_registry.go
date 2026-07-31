package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"

	"go.kenn.io/kit/packstore"
)

const (
	blobStoreKindS3            = "s3"
	blobStoreRoleSecondary     = "secondary"
	blobStoreLifecycleDetached = "detached"
)

var blobStoreNamePattern = regexp.MustCompile(`^[[:graph:]][[:print:]]{0,199}$`)

// BlobStoreStats reports catalog-authorized physical inventory for one store.
type BlobStoreStats struct {
	AuthoritativeObjects int64
	LogicalBytes         int64
	StoredBytes          int64
	PackCount            int64
	DeadPackedBytes      int64
	SoleAuthorityObjects int64
	AffectedDocuments    int64
}

// BlobStoreEvacuationFinalization is the catalog result of revoking an empty
// source after every location has verified destination coverage. Physical
// retirement happens afterward through Kit's reader-safe backend.
type BlobStoreEvacuationFinalization struct {
	Retire           []packstore.ObjectRef
	RevokedLocations int64
	Detached         bool
}

// PrepareSecondaryBlobStore allocates the stable identity and ownership epoch
// that must be published to the physical namespace before RegisterBlobStore
// grants catalog authority.
func (s *Store) PrepareSecondaryBlobStore(name, kind, binding string) (BlobStore, error) {
	if !blobStoreNamePattern.MatchString(name) {
		return BlobStore{}, errors.New("blob-store name must be 1-200 printable characters")
	}
	if kind != blobStoreKindFilesystem && kind != blobStoreKindS3 {
		return BlobStore{}, fmt.Errorf("blob-store kind %q must be filesystem or s3", kind)
	}
	if binding == "" {
		return BlobStore{}, errors.New("blob-store binding is required")
	}
	id, err := newUUIDv4()
	if err != nil {
		return BlobStore{}, fmt.Errorf("creating blob-store identity: %w", err)
	}
	epoch, err := newUUIDv4()
	if err != nil {
		return BlobStore{}, fmt.Errorf("creating blob-store ownership epoch: %w", err)
	}
	return BlobStore{
		ID: id, Name: name, Kind: kind, Role: blobStoreRoleSecondary,
		Lifecycle: blobStoreLifecycleActive, Binding: binding,
		OwnershipEpoch: epoch, CreatedAt: parseStoredTime(nowRFC3339()),
	}, nil
}

// RegisterBlobStore records a namespace only after its ownership marker has
// been durably published and independently read back by the caller.
func (s *Store) RegisterBlobStore(ctx context.Context, candidate BlobStore) error {
	if err := validateBlobStore(candidate); err != nil {
		return err
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM blob_stores WHERE store_id = ? OR name = ?`,
			candidate.ID, candidate.Name,
		).Scan(&count); err != nil {
			return fmt.Errorf("checking blob-store identity: %w", err)
		}
		if count != 0 {
			return fmt.Errorf("blob store %q or %s: %w",
				candidate.Name, candidate.ID, ErrExists)
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO blob_stores(
				store_id, name, kind, role, lifecycle, binding, ownership_epoch, created_at
			) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
			candidate.ID, candidate.Name, candidate.Kind, candidate.Role,
			candidate.Lifecycle, candidate.Binding, candidate.OwnershipEpoch,
			candidate.CreatedAt.UTC().Format(timestampLayout),
		)
		if err != nil {
			return fmt.Errorf("registering blob store %q: %w", candidate.Name, err)
		}
		return nil
	})
}

// BlobStores lists physical-store authority with the fixed primary first.
func (s *Store) BlobStores(ctx context.Context) ([]BlobStore, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT store_id, name, kind, role, lifecycle, binding, ownership_epoch, created_at
		FROM blob_stores
		ORDER BY CASE role WHEN 'primary' THEN 0 ELSE 1 END, name, store_id`)
	if err != nil {
		return nil, fmt.Errorf("listing blob stores: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var stores []BlobStore
	for rows.Next() {
		store, err := scanBlobStore(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning blob store: %w", err)
		}
		stores = append(stores, store)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing blob stores: %w", err)
	}
	return stores, nil
}

// BlobStoreInventory returns per-store authority without inspecting deployment
// namespaces or treating orphan files as catalog objects.
func (s *Store) BlobStoreInventory(
	ctx context.Context,
) (map[string]BlobStoreStats, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.store_id,
		       COUNT(DISTINCT l.blob_hash),
		       COALESCE(SUM(CASE WHEN l.blob_hash IS NOT NULL THEN b.size ELSE 0 END), 0),
		       COALESCE(SUM(CASE
		           WHEN l.kind = 'loose' THEN l.stored_size
		           WHEN l.kind = 'packed' THEN e.stored_len
		           ELSE 0 END), 0),
		       (SELECT COUNT(*) FROM blob_packs p WHERE p.store_id = s.store_id),
		       COALESCE((SELECT SUM(MAX(p.stored_bytes - p.live_stored_bytes, 0))
		                 FROM blob_packs p WHERE p.store_id = s.store_id), 0),
		       (SELECT COUNT(*) FROM blob_locations sole
		        WHERE sole.store_id = s.store_id
		          AND (SELECT COUNT(*) FROM blob_locations peers
		               WHERE peers.blob_hash = sole.blob_hash) = 1),
		       (SELECT COUNT(DISTINCT n.id)
		        FROM nodes n
		        JOIN content_versions v
		          ON v.node_id = n.id AND v.version_id = n.current_version_id
		        JOIN blob_locations sole ON sole.blob_hash = v.blob_hash
		        WHERE n.trashed_at IS NULL
		          AND sole.store_id = s.store_id
		          AND (SELECT COUNT(*) FROM blob_locations peers
		               WHERE peers.blob_hash = sole.blob_hash) = 1)
		FROM blob_stores s
		LEFT JOIN blob_locations l ON l.store_id = s.store_id
		LEFT JOIN blobs b ON b.hash = l.blob_hash
		LEFT JOIN blob_pack_entries e
		  ON e.blob_hash = l.blob_hash AND e.store_id = l.store_id
		GROUP BY s.store_id
		ORDER BY s.store_id`)
	if err != nil {
		return nil, fmt.Errorf("reading blob-store inventory: %w", err)
	}
	defer func() { _ = rows.Close() }()
	inventory := make(map[string]BlobStoreStats)
	for rows.Next() {
		var id string
		var stats BlobStoreStats
		if err := rows.Scan(
			&id, &stats.AuthoritativeObjects, &stats.LogicalBytes, &stats.StoredBytes,
			&stats.PackCount, &stats.DeadPackedBytes, &stats.SoleAuthorityObjects,
			&stats.AffectedDocuments,
		); err != nil {
			return nil, fmt.Errorf("scanning blob-store inventory: %w", err)
		}
		inventory[id] = stats
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading blob-store inventory: %w", err)
	}
	return inventory, nil
}

// BlobStoreUnreadableObjects counts each retained object against every store
// that holds it when none of its catalog locations are currently online.
// Runtime observations stay in Go and never rewrite durable authority.
func (s *Store) BlobStoreUnreadableObjects(
	ctx context.Context, online map[string]bool,
) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT blob_hash,store_id
		FROM blob_locations
		ORDER BY blob_hash,store_id`)
	if err != nil {
		return nil, fmt.Errorf("reading blob locations for health: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make(map[string]int64)
	var currentHash string
	var stores []string
	readable := false
	flush := func() {
		if currentHash == "" || readable {
			return
		}
		for _, storeID := range stores {
			result[storeID]++
		}
	}
	for rows.Next() {
		var hash, storeID string
		if err := rows.Scan(&hash, &storeID); err != nil {
			return nil, fmt.Errorf("scanning blob location health: %w", err)
		}
		if currentHash != "" && hash != currentHash {
			flush()
			stores = stores[:0]
			readable = false
		}
		currentHash = hash
		stores = append(stores, storeID)
		readable = readable || online[storeID]
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading blob location health: %w", err)
	}
	flush()
	return result, nil
}

// BlobStoreBySelector resolves canonical UUIDv4 selectors exclusively as IDs;
// all other selectors are names.
func (s *Store) BlobStoreBySelector(ctx context.Context, selector string) (BlobStore, error) {
	column := SearchMatchName
	if validateUUIDv4(selector) == nil {
		column = "store_id"
	}
	store, err := scanBlobStore(s.db.QueryRowContext(ctx, `
		SELECT store_id, name, kind, role, lifecycle, binding, ownership_epoch, created_at
		FROM blob_stores WHERE `+column+` = ?`, selector))
	if errors.Is(err, sql.ErrNoRows) {
		return BlobStore{}, fmt.Errorf("blob store %q: %w", selector, ErrNotFound)
	}
	if err != nil {
		return BlobStore{}, fmt.Errorf("reading blob store %q: %w", selector, err)
	}
	return store, nil
}

// BeginBlobStoreEvacuation makes one secondary read-only for new placement
// destinations while keeping its current locations readable during copying.
func (s *Store) BeginBlobStoreEvacuation(ctx context.Context, selector string) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		store, err := blobStoreBySelectorTx(ctx, tx, selector)
		if err != nil {
			return err
		}
		if store.Role == blobStoreRolePrimary {
			return ErrBlobStorePrimary
		}
		switch store.Lifecycle {
		case blobStoreLifecycleDraining:
			return nil
		case blobStoreLifecycleActive:
		default:
			return fmt.Errorf("blob store %s is %s: %w",
				store.ID, store.Lifecycle, ErrBlobStoreState)
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE blob_stores SET lifecycle=? WHERE store_id=?`,
			blobStoreLifecycleDraining, store.ID,
		)
		if err != nil {
			return fmt.Errorf("beginning evacuation of blob store %s: %w", store.ID, err)
		}
		return nil
	})
}

// FinalizeBlobStoreEvacuation atomically proves complete destination coverage
// and revokes every source location. A source remains draining while durable
// physical cleanup is pending, then a repeated call detaches it.
func (s *Store) FinalizeBlobStoreEvacuation(
	ctx context.Context, operationID, sourceID, destinationID string,
) (BlobStoreEvacuationFinalization, error) {
	if err := validateUUIDv4(operationID); err != nil {
		return BlobStoreEvacuationFinalization{},
			fmt.Errorf("invalid storage operation ID: %w", err)
	}
	var result BlobStoreEvacuationFinalization
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var operationKind, operationSource, operationState, operationCursor string
		var cancelRequested bool
		if err := tx.QueryRowContext(ctx, `
			SELECT kind,COALESCE(source_store_id,''),state,cancel_requested,cursor
			FROM storage_operations
			WHERE operation_id=?`,
			operationID,
		).Scan(
			&operationKind, &operationSource, &operationState,
			&cancelRequested, &operationCursor,
		); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("storage operation %s: %w", operationID, ErrNotFound)
			}
			return fmt.Errorf("reading evacuation operation %s: %w", operationID, err)
		}
		if operationKind != storageOperationKindEvacuate || operationSource != sourceID {
			return fmt.Errorf(
				"storage operation %s does not evacuate blob store %s: %w",
				operationID, sourceID, ErrBlobStoreState,
			)
		}
		if cancelRequested && operationCursor != storageOperationFinalizingCursor {
			return ErrStorageOperationCancelled
		}
		if StorageOperationState(operationState) != StorageOperationRunning {
			return fmt.Errorf(
				"storage operation %s is %s: %w",
				operationID, operationState, ErrStorageOperationTerminal,
			)
		}
		source, err := blobStoreBySelectorTx(ctx, tx, sourceID)
		if err != nil {
			return err
		}
		if source.Role == blobStoreRolePrimary {
			return ErrBlobStorePrimary
		}
		if source.Lifecycle == blobStoreLifecycleDetached {
			if err := markStorageOperationFinalizingTx(
				ctx, tx, operationID,
			); err != nil {
				return err
			}
			result.Detached = true
			return nil
		}
		if source.Lifecycle != blobStoreLifecycleDraining {
			return fmt.Errorf("blob store %s is %s: %w",
				source.ID, source.Lifecycle, ErrBlobStoreState)
		}
		destination, err := blobStoreBySelectorTx(ctx, tx, destinationID)
		if err != nil {
			return err
		}
		if destination.Lifecycle != blobStoreLifecycleActive {
			return fmt.Errorf("destination blob store %s is %s: %w",
				destination.ID, destination.Lifecycle, ErrBlobStoreState)
		}
		var uncovered int64
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM blob_locations source
			LEFT JOIN blob_locations destination
			  ON destination.blob_hash=source.blob_hash
			 AND destination.store_id=?
			WHERE source.store_id=? AND destination.blob_hash IS NULL`,
			destination.ID, source.ID,
		).Scan(&uncovered); err != nil {
			return fmt.Errorf("checking evacuation coverage for %s: %w", source.ID, err)
		}
		if uncovered != 0 {
			return fmt.Errorf("blob store %s has %d uncovered location(s): %w",
				source.ID, uncovered, ErrBlobStoreNotEmpty)
		}
		refs, err := blobStoreObjectRefsTx(ctx, tx, source.ID)
		if err != nil {
			return err
		}
		if err := recordStorageOperationCleanupTx(
			ctx, tx, operationID, source.ID, refs,
		); err != nil {
			return err
		}
		if err := adoptBlobStoreCleanupTx(
			ctx, tx, operationID, source.ID,
		); err != nil {
			return err
		}
		if err := markStorageOperationFinalizingTx(
			ctx, tx, operationID,
		); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM blob_locations WHERE store_id=?`, source.ID,
		).Scan(&result.RevokedLocations); err != nil {
			return fmt.Errorf("counting evacuated locations for %s: %w", source.ID, err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM blob_pack_entries WHERE store_id=?`, source.ID,
		); err != nil {
			return fmt.Errorf("revoking evacuated pack entries for %s: %w", source.ID, err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM blob_packs WHERE store_id=?`, source.ID,
		); err != nil {
			return fmt.Errorf("revoking evacuated packs for %s: %w", source.ID, err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM blob_locations WHERE store_id=?`, source.ID,
		); err != nil {
			return fmt.Errorf("revoking evacuated locations for %s: %w", source.ID, err)
		}
		var pendingCleanup int64
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM storage_operation_cleanup
			WHERE store_id=?`,
			source.ID,
		).Scan(&pendingCleanup); err != nil {
			return fmt.Errorf("checking evacuated cleanup for %s: %w", source.ID, err)
		}
		if pendingCleanup == 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE blob_stores SET lifecycle=? WHERE store_id=?`,
				blobStoreLifecycleDetached, source.ID,
			); err != nil {
				return fmt.Errorf("detaching evacuated blob store %s: %w", source.ID, err)
			}
			result.Detached = true
		}
		result.Retire = refs
		return nil
	})
	return result, err
}

// adoptBlobStoreCleanupTx gives the finalizing evacuation ownership of every
// already-authorized retirement against its source. The evacuation runner can
// then finish those physical deletions before detaching the binding.
func adoptBlobStoreCleanupTx(
	ctx context.Context, tx *sql.Tx, operationID, storeID string,
) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO storage_operation_cleanup(
			operation_id,store_id,loose_hash,loose_encoding,pack_id
		)
		SELECT ?,store_id,loose_hash,loose_encoding,pack_id
		FROM storage_operation_cleanup
		WHERE store_id=? AND operation_id<>?`,
		operationID, storeID, operationID,
	); err != nil {
		return fmt.Errorf("adopting blob store cleanup for %s: %w", storeID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM storage_operation_cleanup
		WHERE store_id=? AND operation_id<>?`,
		storeID, operationID,
	); err != nil {
		return fmt.Errorf("transferring blob store cleanup for %s: %w", storeID, err)
	}
	return nil
}

func markStorageOperationFinalizingTx(
	ctx context.Context, tx *sql.Tx, operationID string,
) error {
	update, err := tx.ExecContext(ctx, `
		UPDATE storage_operations SET cursor=?,updated_at=?
		WHERE operation_id=? AND state=?`,
		storageOperationFinalizingCursor, nowRFC3339(),
		operationID, StorageOperationRunning,
	)
	return requireOneStorageOperationRow(
		update, err, operationID, "marking finalizing",
	)
}

func blobStoreObjectRefsTx(
	ctx context.Context, tx *sql.Tx, storeID string,
) ([]packstore.ObjectRef, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT l.blob_hash,l.kind,l.encoding
		FROM blob_locations l
		WHERE l.store_id=?
		ORDER BY CASE l.kind WHEN 'loose' THEN 0 ELSE 1 END,l.blob_hash`,
		storeID,
	)
	if err != nil {
		return nil, fmt.Errorf("reading evacuated physical objects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var refs []packstore.ObjectRef
	for rows.Next() {
		var hash, kind string
		var encoding sql.NullString
		if err := rows.Scan(&hash, &kind, &encoding); err != nil {
			return nil, fmt.Errorf("scanning evacuated physical object: %w", err)
		}
		switch kind {
		case blobLocationKindLoose:
			var value packstore.LooseEncoding
			switch encoding.String {
			case looseEncodingRaw:
				value = packstore.LooseEncodingRaw
			case looseEncodingZstd:
				value = packstore.LooseEncodingZstd
			default:
				return nil, fmt.Errorf("invalid loose encoding %q", encoding.String)
			}
			refs = append(refs, packstore.ObjectRef{
				LooseHash: packstore.Hash(hash), LooseEncoding: value,
			})
		case blobLocationKindPacked:
			// Pack containers are enumerated independently below. Their
			// per-blob mappings may already have been revoked by placement.
		default:
			return nil, fmt.Errorf("invalid location kind %q", kind)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading evacuated physical objects: %w", err)
	}
	packRows, err := tx.QueryContext(ctx, `
		SELECT pack_id FROM blob_packs
		WHERE store_id=?
		ORDER BY pack_id`,
		storeID,
	)
	if err != nil {
		return nil, fmt.Errorf("reading evacuated pack objects: %w", err)
	}
	defer func() { _ = packRows.Close() }()
	for packRows.Next() {
		var packID string
		if err := packRows.Scan(&packID); err != nil {
			return nil, fmt.Errorf("scanning evacuated pack object: %w", err)
		}
		refs = append(refs, packstore.ObjectRef{PackID: packID})
	}
	if err := packRows.Err(); err != nil {
		return nil, fmt.Errorf("reading evacuated pack objects: %w", err)
	}
	return refs, nil
}

// DetachBlobStore removes an empty secondary from ordinary backend admission
// while retaining its stable catalog identity for explicit unregister.
func (s *Store) DetachBlobStore(ctx context.Context, selector string) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		store, err := blobStoreBySelectorTx(ctx, tx, selector)
		if err != nil {
			return err
		}
		if store.Role == blobStoreRolePrimary {
			return ErrBlobStorePrimary
		}
		if store.Lifecycle == blobStoreLifecycleDetached {
			return nil
		}
		activeOperations, err := activeStorageOperationsForStoreTx(ctx, tx, store.ID)
		if err != nil {
			return fmt.Errorf(
				"checking active operations for blob store %s: %w", store.ID, err,
			)
		}
		if activeOperations != 0 {
			return fmt.Errorf(
				"blob store %s has %d active operation(s): %w",
				store.ID, activeOperations, ErrBlobStoreState,
			)
		}
		var locations, packs, cleanups int
		if err := tx.QueryRowContext(ctx, `
			SELECT
				(SELECT COUNT(*) FROM blob_locations WHERE store_id = ?),
				(SELECT COUNT(*) FROM blob_packs WHERE store_id = ?),
				(SELECT COUNT(*) FROM storage_operation_cleanup WHERE store_id = ?)`,
			store.ID, store.ID, store.ID,
		).Scan(&locations, &packs, &cleanups); err != nil {
			return fmt.Errorf("checking blob store %s contents: %w", store.ID, err)
		}
		if locations != 0 || packs != 0 || cleanups != 0 {
			return fmt.Errorf(
				"blob store %s has %d location(s), %d pack(s), and %d pending cleanup(s): %w",
				store.ID, locations, packs, cleanups, ErrBlobStoreNotEmpty,
			)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE blob_stores SET lifecycle = ? WHERE store_id = ?`,
			blobStoreLifecycleDetached, store.ID); err != nil {
			return fmt.Errorf("detaching blob store %s: %w", store.ID, err)
		}
		return nil
	})
}

// UnregisterBlobStore removes an already detached, empty secondary identity.
func (s *Store) UnregisterBlobStore(ctx context.Context, selector string) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		store, err := blobStoreBySelectorTx(ctx, tx, selector)
		if err != nil {
			return err
		}
		if store.Role == blobStoreRolePrimary {
			return ErrBlobStorePrimary
		}
		if store.Lifecycle != blobStoreLifecycleDetached {
			return fmt.Errorf("blob store %s is %s: %w",
				store.ID, store.Lifecycle, ErrBlobStoreState)
		}
		activeOperations, err := activeStorageOperationsForStoreTx(ctx, tx, store.ID)
		if err != nil {
			return fmt.Errorf(
				"checking active operations for blob store %s: %w", store.ID, err,
			)
		}
		if activeOperations != 0 {
			return fmt.Errorf(
				"blob store %s has %d active operation(s): %w",
				store.ID, activeOperations, ErrBlobStoreState,
			)
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM blob_stores WHERE store_id = ?`, store.ID)
		if err != nil {
			return fmt.Errorf("unregistering blob store %s: %w", store.ID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("reading unregistered blob store %s result: %w", store.ID, err)
		}
		if affected != 1 {
			return fmt.Errorf("unregistering blob store %s affected %d rows", store.ID, affected)
		}
		return nil
	})
}

func blobStoreBySelectorTx(
	ctx context.Context, tx *sql.Tx, selector string,
) (BlobStore, error) {
	column := SearchMatchName
	if validateUUIDv4(selector) == nil {
		column = "store_id"
	}
	store, err := scanBlobStore(tx.QueryRowContext(ctx, `
		SELECT store_id, name, kind, role, lifecycle, binding, ownership_epoch, created_at
		FROM blob_stores WHERE `+column+` = ?`, selector))
	if errors.Is(err, sql.ErrNoRows) {
		return BlobStore{}, fmt.Errorf("blob store %q: %w", selector, ErrNotFound)
	}
	return store, err
}

func validateBlobStore(store BlobStore) error {
	if err := validateUUIDv4(store.ID); err != nil {
		return fmt.Errorf("invalid blob-store identity: %w", err)
	}
	if err := validateUUIDv4(store.OwnershipEpoch); err != nil {
		return fmt.Errorf("invalid blob-store ownership epoch: %w", err)
	}
	if !blobStoreNamePattern.MatchString(store.Name) {
		return errors.New("blob-store name must be 1-200 printable characters")
	}
	if store.Kind != blobStoreKindFilesystem && store.Kind != blobStoreKindS3 {
		return fmt.Errorf("invalid blob-store kind %q", store.Kind)
	}
	if store.Role != blobStoreRoleSecondary ||
		store.Lifecycle != blobStoreLifecycleActive ||
		store.Binding == "" || store.CreatedAt.IsZero() {
		return errors.New("invalid secondary blob-store registration")
	}
	return nil
}
