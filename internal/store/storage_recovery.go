package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"go.kenn.io/kit/packstore"
)

// StorageRecoveryPlan binds one explicit repair or fenced-store salvage to
// immutable content identity and the exact physical candidates reviewed.
type StorageRecoveryPlan struct {
	Version     int                     `json:"version"`
	Kind        string                  `json:"kind"`
	Digest      string                  `json:"digest"`
	Hash        string                  `json:"hash"`
	Size        int64                   `json:"size"`
	Source      packstore.ReadLocation  `json:"source"`
	Destination string                  `json:"destination"`
	Prior       *packstore.ReadLocation `json:"prior,omitempty"`
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
	var source packstore.ReadLocation
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
		if source.StoreID == "" &&
			(requiredSource == "" || string(location.StoreID) == requiredSource) {
			source = location
		}
	}
	if err := rows.Err(); err != nil {
		return StorageRecoveryPlan{}, fmt.Errorf("reading recovery locations: %w", err)
	}
	if source.StoreID == "" {
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
		Source: source, Destination: destination.ID, Prior: prior,
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
	if err := plan.Source.Validate(); err != nil {
		return fmt.Errorf("validating storage recovery source: %w", err)
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

// CommitStorageRecovery grants the fully verified replacement generation.
func (s *Store) CommitStorageRecovery(
	ctx context.Context, plan StorageRecoveryPlan, receipt packstore.ReadLocation,
) error {
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
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var size int64
		if err := tx.QueryRowContext(ctx,
			`SELECT size FROM blobs WHERE hash=?`, plan.Hash,
		).Scan(&size); err != nil {
			return fmt.Errorf("rechecking storage recovery membership: %w", err)
		}
		if size != plan.Size || receipt.Loose.LogicalSize != plan.Size {
			return packstore.ErrPhysicalCorrupt
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
		return nil
	})
}
