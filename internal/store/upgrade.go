package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/kit/pack"

	docsqlite "go.kenn.io/docbank/pkg/sqlite"
)

const (
	v090BackupSuffix   = ".v0.9.0.bak"
	upgradeStageSuffix = ".upgrade-staging"
	upgradeJSONLSuffix = ".upgrade-metadata.jsonl"
)

type databaseSchema uint8

const (
	schemaFresh databaseSchema = iota
	schemaCurrent
	schemaV090
)

var renameUpgradeFile = os.Rename

// prepareReleasedSchemaUpgrade recognizes only storage layouts that shipped in
// a public release. The v0.9.0 cutover rebuilds the database through the same
// deterministic JSONL authority used by backup and restore; it never mutates
// the released schema in place.
func prepareReleasedSchemaUpgrade(path string, driver docsqlite.Driver) error {
	if err := recoverInterruptedV090Upgrade(path, driver); err != nil {
		return fmt.Errorf("recovering interrupted v0.9.0 database upgrade: %w", err)
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspecting database before open: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("opening database: %s is not a regular file", path)
	}
	if info.Size() == 0 {
		return nil
	}

	db, err := driver.Open(path, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Deferred,
	})
	if err != nil {
		return fmt.Errorf("inspecting database schema with %s: %w", driver.Name(), err)
	}
	kind, classifyErr := classifyDatabaseSchema(db)
	closeErr := db.Close()
	if classifyErr != nil || closeErr != nil {
		return errors.Join(classifyErr, closeErr)
	}
	switch kind {
	case schemaFresh, schemaCurrent:
		return cleanupStaleUpgradeFiles(path)
	case schemaV090:
		return cutoverV090Database(path, driver)
	default:
		return errors.New("opening database: unsupported schema")
	}
}

func classifyDatabaseSchema(db *sql.DB) (databaseSchema, error) {
	blobs, err := tableColumns(db, "blobs")
	if err != nil {
		return 0, err
	}
	if len(blobs) == 0 {
		return schemaFresh, nil
	}
	packs, err := tableColumns(db, "blob_packs")
	if err != nil {
		return 0, err
	}
	currentBlobColumns := []string{
		metadataCreatedAtField, "hash", "loose_encoding", "loose_stored_size", "pack_eligible", "size",
	}
	currentPackColumns := []string{
		metadataCreatedAtField, "entry_count", "live_entries", "live_raw_bytes", "live_stored_bytes",
		"max_live_raw_len", "max_live_stored_len", "pack_id", "scan_hash", "stored_bytes",
	}
	v090BlobColumns := []string{metadataCreatedAtField, "hash", "size"}
	v090PackColumns := []string{metadataCreatedAtField, "entry_count", "pack_id", "stored_bytes"}
	switch {
	case slices.Equal(blobs, currentBlobColumns) && slices.Equal(packs, currentPackColumns):
		return schemaCurrent, nil
	case slices.Equal(blobs, v090BlobColumns) && slices.Equal(packs, v090PackColumns):
		if err := requireV090Tables(db); err != nil {
			return 0, err
		}
		return schemaV090, nil
	default:
		return 0, fmt.Errorf(
			"opening database: unsupported schema (blobs=%s blob_packs=%s)",
			strings.Join(blobs, ","), strings.Join(packs, ","),
		)
	}
}

func tableColumns(db *sql.DB, table string) ([]string, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?) ORDER BY name`, table)
	if err != nil {
		return nil, fmt.Errorf("reading %s schema: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("reading %s column: %w", table, err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading %s columns: %w", table, err)
	}
	return columns, nil
}

func requireV090Tables(db *sql.DB) error {
	required := []string{
		"audit_authority", "audit_baselines", "audit_memberships", "audit_records", "audit_scopes",
		"blob_pack_index", "blobs", "content_versions", "extracted_text", "ingests", "node_tags",
		"nodes", "provenance", "tags", "vault_metadata", "watch_sources",
	}
	for _, table := range required {
		var found bool
		if err := db.QueryRow(`SELECT EXISTS(
			SELECT 1 FROM sqlite_master WHERE type='table' AND name=?
		)`, table).Scan(&found); err != nil {
			return fmt.Errorf("recognizing v0.9.0 schema table %s: %w", table, err)
		}
		if !found {
			return fmt.Errorf("opening database: unsupported schema; missing v0.9.0 table %s", table)
		}
	}
	return nil
}

func cutoverV090Database(path string, driver docsqlite.Driver) (err error) {
	backupPath := path + v090BackupSuffix
	stagePath := path + upgradeStageSuffix
	jsonlPath := path + upgradeJSONLSuffix
	if _, statErr := os.Stat(backupPath); statErr == nil {
		return fmt.Errorf("upgrading v0.9.0 database: recovery copy already exists at %s", backupPath)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("checking v0.9.0 recovery copy: %w", statErr)
	}
	if err := removeUpgradeFileSet(stagePath); err != nil {
		return err
	}
	if err := removeIfExists(jsonlPath); err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			err = errors.Join(err, removeUpgradeFileSet(stagePath), removeIfExists(jsonlPath))
		}
	}()

	source, err := openV090Source(path, driver)
	if err != nil {
		return err
	}
	snapshot, err := source.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		_ = source.Close()
		return fmt.Errorf("pinning v0.9.0 metadata: %w", err)
	}
	closeSource := func() error {
		return errors.Join(snapshot.Rollback(), source.Close())
	}

	if err := writeUpgradeJSONL(snapshot, jsonlPath); err != nil {
		_ = closeSource()
		return err
	}
	target, err := openCurrentStore(stagePath, driver)
	if err != nil {
		_ = closeSource()
		return fmt.Errorf("creating current database for v0.9.0 upgrade: %w", err)
	}
	if err := importUpgradeJSONL(target, jsonlPath); err != nil {
		_ = target.Close()
		_ = closeSource()
		return err
	}
	if err := restoreV090PhysicalCatalog(context.Background(), snapshot, target); err != nil {
		_ = target.Close()
		_ = closeSource()
		return err
	}
	if err := target.ValidateMetadata(context.Background()); err != nil {
		_ = target.Close()
		_ = closeSource()
		return fmt.Errorf("validating upgraded v0.9.0 metadata: %w", err)
	}
	if err := target.Checkpoint(context.Background()); err != nil {
		_ = target.Close()
		_ = closeSource()
		return fmt.Errorf("checkpointing upgraded v0.9.0 database: %w", err)
	}
	if err := target.Close(); err != nil {
		_ = closeSource()
		return fmt.Errorf("closing upgraded v0.9.0 database: %w", err)
	}
	if err := closeSource(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return fmt.Errorf("closing v0.9.0 source database: %w", err)
	}
	if err := syncRegularFile(stagePath); err != nil {
		return err
	}
	if err := removeUpgradeFileSetSidecars(stagePath); err != nil {
		return err
	}
	if err := removeIfExists(jsonlPath); err != nil {
		return err
	}
	if err := syncUpgradeDirectory(path); err != nil {
		return fmt.Errorf("syncing v0.9.0 upgrade staging: %w", err)
	}
	if err := publishV090Upgrade(path, stagePath, backupPath); err != nil {
		return err
	}
	published = true
	return nil
}

func openV090Source(path string, driver docsqlite.Driver) (*sql.DB, error) {
	db, err := driver.Open(path, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Deferred,
	})
	if err != nil {
		return nil, fmt.Errorf("opening v0.9.0 source database: %w", err)
	}
	var busy, logFrames, checkpointed int64
	if err := db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(
		&busy, &logFrames, &checkpointed,
	); err != nil || busy != 0 || logFrames != 0 {
		_ = db.Close()
		if err != nil {
			return nil, fmt.Errorf("checkpointing v0.9.0 source database: %w", err)
		}
		return nil, fmt.Errorf(
			"checkpointing v0.9.0 source database: WAL remains busy=%d log=%d checkpointed=%d",
			busy, logFrames, checkpointed,
		)
	}
	return db, nil
}

func writeUpgradeJSONL(snapshot *sql.Tx, path string) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("creating v0.9.0 upgrade metadata: %w", err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	if err := exportMetadataSnapshot(context.Background(), snapshot, f); err != nil {
		return fmt.Errorf("exporting v0.9.0 metadata: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing v0.9.0 upgrade metadata: %w", err)
	}
	return nil
}

func importUpgradeJSONL(target *Store, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening v0.9.0 upgrade metadata: %w", err)
	}
	if err := target.ImportMetadata(context.Background(), f); err != nil {
		_ = f.Close()
		return fmt.Errorf("importing v0.9.0 metadata: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing v0.9.0 upgrade metadata: %w", err)
	}
	return nil
}

func restoreV090PhysicalCatalog(ctx context.Context, source metadataQuerier, target *Store) error {
	return target.withStorageTx(ctx, func(tx *sql.Tx) error {
		packIDs, err := restoreV090PackRecords(ctx, source, tx)
		if err != nil {
			return err
		}
		if err := restoreV090PackMappings(ctx, source, tx); err != nil {
			return err
		}
		for _, packID := range packIDs {
			if err := finalizeV090PackScanHash(ctx, tx, packID); err != nil {
				return err
			}
		}
		return nil
	})
}

func finalizeV090PackScanHash(ctx context.Context, tx *sql.Tx, packID string) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE blob_packs
		SET scan_hash = COALESCE((
			SELECT MIN(blob_hash) FROM blob_pack_index WHERE pack_id = ?
		), '')
		WHERE pack_id = ?`, packID, packID)
	if err != nil {
		return fmt.Errorf("finalizing restored v0.9.0 pack %s: %w", packID, err)
	}
	return requireOneRow(result, "finalizing restored v0.9.0 pack "+packID)
}

func restoreV090PackRecords(
	ctx context.Context, source metadataQuerier, tx *sql.Tx,
) ([]string, error) {
	rows, err := source.QueryContext(ctx, `
		SELECT pack_id, entry_count, stored_bytes, created_at
		FROM blob_packs ORDER BY created_at, pack_id`)
	if err != nil {
		return nil, fmt.Errorf("reading v0.9.0 pack records: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var packIDs []string
	for rows.Next() {
		record, err := scanPackRecord(rows)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO blob_packs(pack_id, entry_count, stored_bytes, created_at)
			VALUES(?, ?, ?, ?)`, record.PackID, record.EntryCount, record.StoredBytes,
			record.CreatedAt.UTC().Format(timestampLayout)); err != nil {
			return nil, fmt.Errorf("restoring v0.9.0 pack %s: %w", record.PackID, err)
		}
		packIDs = append(packIDs, record.PackID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading v0.9.0 pack records: %w", err)
	}
	return packIDs, nil
}

func restoreV090PackMappings(ctx context.Context, source metadataQuerier, tx *sql.Tx) error {
	rows, err := source.QueryContext(ctx, `
		SELECT blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c
		FROM blob_pack_index ORDER BY blob_hash`)
	if err != nil {
		return fmt.Errorf("reading v0.9.0 pack mappings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		entry, err := scanPackEntry(rows)
		if err != nil {
			return err
		}
		if err := writeAdoption(ctx, tx, entry, false); err != nil {
			return fmt.Errorf("restoring v0.9.0 packed blob %s: %w", entry.Hash, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading v0.9.0 pack mappings: %w", err)
	}
	return nil
}

func publishV090Upgrade(path, stagePath, backupPath string) error {
	if err := removeUpgradeFileSetSidecars(path); err != nil {
		return err
	}
	if err := syncRegularFile(path); err != nil {
		return err
	}
	if err := renameUpgradeFile(path, backupPath); err != nil {
		return fmt.Errorf("retaining v0.9.0 recovery database: %w", err)
	}
	if err := syncUpgradeDirectory(path); err != nil {
		_ = renameUpgradeFile(backupPath, path)
		return fmt.Errorf("syncing v0.9.0 recovery database: %w", err)
	}
	if err := renameUpgradeFile(stagePath, path); err != nil {
		rollbackErr := renameUpgradeFile(backupPath, path)
		return errors.Join(fmt.Errorf("publishing upgraded database: %w", err), rollbackErr)
	}
	if err := syncUpgradeDirectory(path); err != nil {
		return fmt.Errorf("syncing upgraded database publication: %w", err)
	}
	return nil
}

func recoverInterruptedV090Upgrade(path string, driver docsqlite.Driver) error {
	_, pathErr := os.Stat(path)
	if pathErr == nil {
		return nil
	}
	if !errors.Is(pathErr, os.ErrNotExist) {
		return pathErr
	}
	backupPath := path + v090BackupSuffix
	stagePath := path + upgradeStageSuffix
	_, backupErr := os.Stat(backupPath)
	_, stageErr := os.Stat(stagePath)
	if backupErr == nil && stageErr == nil {
		if err := validateUpgradeStage(stagePath, driver); err != nil {
			if cleanupErr := removeUpgradeFileSet(stagePath); cleanupErr != nil {
				return errors.Join(err, cleanupErr)
			}
			if renameErr := renameUpgradeFile(backupPath, path); renameErr != nil {
				return errors.Join(err, renameErr)
			}
			return syncUpgradeDirectory(path)
		}
		if err := renameUpgradeFile(stagePath, path); err != nil {
			return err
		}
		return syncUpgradeDirectory(path)
	}
	if backupErr == nil && errors.Is(stageErr, os.ErrNotExist) {
		if err := renameUpgradeFile(backupPath, path); err != nil {
			return err
		}
		return syncUpgradeDirectory(path)
	}
	if !errors.Is(backupErr, os.ErrNotExist) {
		return backupErr
	}
	if !errors.Is(stageErr, os.ErrNotExist) {
		return stageErr
	}
	return nil
}

func syncUpgradeDirectory(path string) error {
	if err := pack.SyncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("syncing database upgrade directory: %w", err)
	}
	return nil
}

func validateUpgradeStage(path string, driver docsqlite.Driver) error {
	db, err := driver.Open(path, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Deferred,
	})
	if err != nil {
		return fmt.Errorf("opening interrupted upgrade staging database: %w", err)
	}
	kind, classifyErr := classifyDatabaseSchema(db)
	closeErr := db.Close()
	if classifyErr != nil || closeErr != nil {
		return errors.Join(classifyErr, closeErr)
	}
	if kind != schemaCurrent {
		return errors.New("interrupted upgrade staging database does not use the current schema")
	}
	store, err := openCurrentStore(path, driver)
	if err != nil {
		return fmt.Errorf("opening interrupted upgrade staging store: %w", err)
	}
	validateErr := store.ValidateMetadata(context.Background())
	closeErr = store.Close()
	if validateErr != nil || closeErr != nil {
		return errors.Join(validateErr, closeErr)
	}
	return nil
}

func cleanupStaleUpgradeFiles(path string) error {
	return errors.Join(
		removeUpgradeFileSet(path+upgradeStageSuffix),
		removeIfExists(path+upgradeJSONLSuffix),
	)
}

func removeUpgradeFileSet(path string) error {
	return errors.Join(removeIfExists(path), removeUpgradeFileSetSidecars(path))
}

func removeUpgradeFileSetSidecars(path string) error {
	return errors.Join(removeIfExists(path+"-wal"), removeIfExists(path+"-shm"))
}

func removeIfExists(path string) error {
	err := os.Remove(path)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("removing %s: %w", path, err)
}

func syncRegularFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening %s for sync: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("syncing %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing %s after sync: %w", path, err)
	}
	return nil
}
