package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"uuid"

	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/store/pgschema"
)

var (
	ErrPostgresPrimaryBound   = errors.New("postgres catalog already has bound primary storage")
	ErrPostgresPrimaryUnbound = errors.New("postgres catalog has no bound primary storage")
)

// InitializePostgresPrimary installs a fresh storage binding or verifies the
// existing binding. The publication callback runs under the catalog writer lock.
func InitializePostgresPrimary(
	ctx context.Context, db *sql.DB, cfg pgschema.Config, existing bool,
	publish func(context.Context, packstore.Ownership) error,
) (uuid.UUID, error) {
	if _, err := pgschema.Install(ctx, db, cfg); err != nil {
		return uuid.Nil(), err
	}
	s := &Store{db: db, writeDB: db, postgresSchema: cfg.Schema}
	var id uuid.UUID
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if err := selectPostgresStorageSchema(ctx, tx, cfg.Schema); err != nil {
			return err
		}
		var bound bool
		var storeID, epoch string
		if err := tx.QueryRowContext(ctx, `SELECT v.vault_uid,v.primary_storage_bound,b.store_id,b.ownership_epoch
			FROM vault_metadata v CROSS JOIN blob_stores b
			WHERE v.singleton=1 AND b.role='primary'`).Scan(&id, &bound, &storeID, &epoch); err != nil {
			return fmt.Errorf("reading postgres primary storage: %w", err)
		}
		switch {
		case bound && existing:
			return publish(ctx, packstore.Ownership{
				Format: packstore.OwnershipFormatV1, Vault: id.String(),
				Store: packstore.StoreID(storeID), Epoch: epoch,
			})
		case bound:
			return ErrPostgresPrimaryBound
		case existing:
			return ErrPostgresPrimaryUnbound
		}
		if err := requirePristineMetadataTarget(ctx, tx, true); err != nil {
			return err
		}
		ownership, err := bindPostgresPrimaryTx(ctx, tx)
		if err != nil {
			return err
		}
		return publish(ctx, ownership)
	})
	if err != nil {
		return uuid.Nil(), err
	}
	return id, nil
}

func bindPostgresPrimaryTx(ctx context.Context, tx *sql.Tx) (packstore.Ownership, error) {
	// A failed commit can leave files on disk. Only this attempt's epoch
	// becomes authoritative when the catalog commits.
	epoch, err := newUUIDv4()
	if err != nil {
		return packstore.Ownership{}, err
	}
	var vaultID, storeID string
	if err := tx.QueryRowContext(ctx, `UPDATE vault_metadata SET primary_storage_bound=TRUE
		WHERE singleton=1 RETURNING vault_uid`).Scan(&vaultID); err != nil {
		return packstore.Ownership{}, fmt.Errorf("binding postgres primary storage: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `UPDATE blob_stores SET ownership_epoch=$1
		WHERE role='primary' RETURNING store_id`, epoch).Scan(&storeID); err != nil {
		return packstore.Ownership{}, fmt.Errorf("assigning postgres primary storage epoch: %w", err)
	}
	return packstore.Ownership{
		Format: packstore.OwnershipFormatV1, Vault: vaultID,
		Store: packstore.StoreID(storeID), Epoch: epoch,
	}, nil
}
