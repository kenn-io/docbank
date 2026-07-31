package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	blobStoreKindFilesystem    = "filesystem"
	blobStoreRolePrimary       = "primary"
	blobStoreLifecycleActive   = "active"
	blobStoreLifecycleDraining = "draining"
	primaryBlobStoreName       = "primary"
	primaryBlobStoreBinding    = "builtin-primary"
)

// BlobStore is one stable physical-store identity in the local placement
// catalog. Binding names refer to machine-local configuration; their resolved
// paths or credentials never enter SQLite.
type BlobStore struct {
	ID             string
	Name           string
	Kind           string
	Role           string
	Lifecycle      string
	Binding        string
	OwnershipEpoch string
	CreatedAt      time.Time
}

func ensurePrimaryBlobStoreTx(tx *sql.Tx) (BlobStore, error) {
	store, err := primaryBlobStoreTx(tx)
	if err == nil {
		return store, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return BlobStore{}, err
	}
	id, err := newUUIDv4()
	if err != nil {
		return BlobStore{}, fmt.Errorf("creating primary blob-store identity: %w", err)
	}
	epoch, err := newUUIDv4()
	if err != nil {
		return BlobStore{}, fmt.Errorf("creating primary ownership epoch: %w", err)
	}
	createdAt := nowRFC3339()
	if _, err := tx.Exec(`
		INSERT INTO blob_stores(
			store_id, name, kind, role, lifecycle, binding, ownership_epoch, created_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		id, primaryBlobStoreName, blobStoreKindFilesystem, blobStoreRolePrimary,
		blobStoreLifecycleActive, primaryBlobStoreBinding, epoch, createdAt,
	); err != nil {
		return BlobStore{}, fmt.Errorf("creating primary blob store: %w", err)
	}
	return BlobStore{
		ID: id, Name: primaryBlobStoreName, Kind: blobStoreKindFilesystem,
		Role: blobStoreRolePrimary, Lifecycle: blobStoreLifecycleActive,
		Binding: primaryBlobStoreBinding, OwnershipEpoch: epoch,
		CreatedAt: parseStoredTime(createdAt),
	}, nil
}

func primaryBlobStoreTx(tx *sql.Tx) (BlobStore, error) {
	return scanBlobStore(tx.QueryRow(`
		SELECT store_id, name, kind, role, lifecycle, binding, ownership_epoch, created_at
		FROM blob_stores WHERE role = ?`, blobStoreRolePrimary))
}

// PrimaryBlobStore returns the fixed local filesystem store.
func (s *Store) PrimaryBlobStore(ctx context.Context) (BlobStore, error) {
	store, err := scanBlobStore(s.db.QueryRowContext(ctx, `
		SELECT store_id, name, kind, role, lifecycle, binding, ownership_epoch, created_at
		FROM blob_stores WHERE role = ?`, blobStoreRolePrimary))
	if errors.Is(err, sql.ErrNoRows) {
		return BlobStore{}, errors.New("primary blob store is missing")
	}
	return store, err
}

func scanBlobStore(row scanner) (BlobStore, error) {
	var store BlobStore
	var createdAt string
	if err := row.Scan(
		&store.ID, &store.Name, &store.Kind, &store.Role, &store.Lifecycle,
		&store.Binding, &store.OwnershipEpoch, &createdAt,
	); err != nil {
		return BlobStore{}, err
	}
	if err := validateUUIDv4(store.ID); err != nil {
		return BlobStore{}, fmt.Errorf("validating blob-store identity: %w", err)
	}
	if err := validateUUIDv4(store.OwnershipEpoch); err != nil {
		return BlobStore{}, fmt.Errorf("validating blob-store ownership epoch: %w", err)
	}
	created, err := time.Parse(timestampLayout, createdAt)
	if err != nil {
		return BlobStore{}, fmt.Errorf("parsing blob-store creation time: %w", err)
	}
	store.CreatedAt = created
	if store.Name == "" || store.Kind == "" || store.Role == "" ||
		store.Lifecycle == "" || store.Binding == "" {
		return BlobStore{}, errors.New("blob store has an empty required field")
	}
	return store, nil
}

func parseStoredTime(raw string) time.Time {
	value, _ := time.Parse(timestampLayout, raw)
	return value
}
