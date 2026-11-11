package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/docbank/internal/home"

	docsqlite "go.kenn.io/docbank/sqlite"
)

// OpenExisting opens a checkpointed current database for identity, path and blob reads only.
func OpenExisting(ctx context.Context, path string, driver docsqlite.Driver, scratchDir string) (_ *Store, retErr error) {
	if scratchDir == "" {
		return nil, errors.New("schema scratch directory must not be empty")
	}
	scratchDir, err := home.CanonicalRoot(scratchDir)
	if err != nil {
		return nil, fmt.Errorf("resolving schema scratch directory: %w", err)
	}
	inside, err := home.PathWithin(filepath.Dir(path), scratchDir)
	if err != nil {
		return nil, err
	}
	if inside {
		return nil, fmt.Errorf("schema scratch directory is inside vault: %s", scratchDir)
	}
	if err := docsqlite.Validate(driver); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("database missing: %w", ErrNeedsOwner)
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("database %s is not a regular file; repair it before opening", path)
	}
	if info.Size() == 0 {
		return nil, fmt.Errorf("database is empty: %w", ErrNeedsOwner)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		_, err := os.Lstat(path + suffix)
		if err == nil {
			return nil, fmt.Errorf("database sidecar %s exists: %w", suffix, ErrNeedsOwner)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	db, err := driver.Open(path, docsqlite.OpenOptions{Access: docsqlite.ReadOnlyImmutable, TransactionMode: docsqlite.Deferred})
	if err != nil {
		return nil, fmt.Errorf("opening existing database: %w", err)
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, db.Close())
		}
	}()
	schema, err := classifyDatabaseSchema(ctx, driver, db, scratchDir)
	if err != nil {
		return nil, err
	}
	if !schema.current {
		return nil, fmt.Errorf("database requires initialization or upgrade: %w", ErrNeedsOwner)
	}
	s := &Store{db: db, path: path, driver: driver}
	var version int
	if err := db.QueryRowContext(ctx, `SELECT vault_uid, schema_version FROM vault_metadata WHERE singleton = 1`).Scan(&s.vaultID, &version); err != nil {
		return nil, fmt.Errorf("reading vault identity: %w", err)
	}
	if version != currentStorageSchemaVersion {
		return nil, fmt.Errorf("schema version %d does not match binary schema %d", version, currentStorageSchemaVersion)
	}
	if err := validateUUIDv4(s.vaultID); err != nil {
		return nil, fmt.Errorf("validating vault identity: %w", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT id FROM nodes WHERE parent_id IS NULL`).Scan(&s.rootID); err != nil {
		return nil, fmt.Errorf("reading root node: %w", err)
	}
	primary, err := s.PrimaryBlobStore(ctx)
	if err != nil {
		return nil, err
	}
	s.primaryStoreID = primary.ID
	return s, nil
}
