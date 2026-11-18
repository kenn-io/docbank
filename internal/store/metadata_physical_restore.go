package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"

	"go.kenn.io/docbank/internal/store/pgschema"
	"go.kenn.io/kit/packstore"
)

// RestorePostgresMetadata installs verified logical and primary storage records
// from a private restore. publish installs the files with the new identity
// before the transaction commits. The caller owns setup and filesystem cleanup.
func RestorePostgresMetadata(
	ctx context.Context, db *sql.DB, cfg pgschema.Config, input io.Reader, verified *Store,
	publish func(context.Context, packstore.Ownership) error,
) error {
	if _, err := pgschema.Setup(ctx, db, cfg); err != nil {
		return err
	}
	s := &Store{db: db, writeDB: db, postgresSchema: cfg.Schema}
	return s.importMetadata(ctx, input, func(tx *sql.Tx, vaultID string) error {
		primary, err := primaryBlobStoreTx(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.restorePostgresStorage(ctx, tx, verified, primary.ID); err != nil {
			return err
		}
		return publish(ctx, packstore.Ownership{
			Format: packstore.OwnershipFormatV1, Vault: vaultID,
			Store: packstore.StoreID(primary.ID), Epoch: primary.OwnershipEpoch,
		})
	})
}

func (s *Store) restorePostgresStorage(ctx context.Context, tx *sql.Tx, verified *Store, storeID string) error {
	catalog := NewPackCatalog(verified)
	records, err := catalog.ListPackRecords(ctx)
	if err != nil {
		return err
	}
	entries, err := catalog.ListIndexed(ctx)
	if err != nil {
		return err
	}
	adoptions := make([]packstore.Adoption, 0, len(entries))
	for _, entry := range entries {
		adoptions = append(adoptions, packstore.Adoption{Entry: entry})
	}
	if err := insertRestoredPacksTx(ctx, tx, storeID, records, adoptions); err != nil {
		return err
	}
	keys := make([][2]string, 0, len(records))
	for _, record := range records {
		keys = append(keys, [2]string{storeID, record.PackID})
	}
	if err := s.refreshPackSummariesTx(ctx, tx, keys); err != nil {
		return err
	}
	if err := queryRows(ctx, verified.db, `SELECT blob_hash, encoding, stored_size, pack_eligible
		FROM blob_locations WHERE store_id=$1 AND kind='loose'`, []any{verified.primaryStoreID}, func(rows *sql.Rows) error {
		var hash string
		var physical BlobPhysical
		if err := rows.Scan(&hash, &physical.Encoding, &physical.StoredBytes, &physical.PackEligible); err != nil {
			return err
		}
		return writeLooseLocationTx(ctx, tx, storeID, hash, physical)
	}); err != nil {
		return fmt.Errorf("restoring loose blob authority: %w", err)
	}
	var missing string
	err = tx.QueryRowContext(ctx, `SELECT b.hash FROM blobs b
		WHERE NOT EXISTS (SELECT 1 FROM blob_locations l WHERE l.store_id=$1 AND l.blob_hash=b.hash)
		ORDER BY b.hash LIMIT 1`, storeID).Scan(&missing)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("verifying restored primary storage coverage: %w", err)
	}
	return fmt.Errorf("restored blob %s has no primary storage authority", missing)
}
