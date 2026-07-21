package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/kit/pack"

	docsqlite "go.kenn.io/docbank/pkg/sqlite"
)

const v090BackupSuffix = ".v0.9.0.bak"

type databaseSchema struct {
	version int
	fresh   bool
	current bool
	source  *releasedStorageSchema
}

type releasedStorageSchema struct {
	version         int
	release         string
	backupSuffix    string
	validate        func(*sql.DB, []string, []string) error
	exportMetadata  func(context.Context, *sql.Tx, io.Writer) error
	restorePhysical func(context.Context, metadataQuerier, *Store) error
}

var releasedStorageSchemas = []releasedStorageSchema{
	{
		version: 1, release: "v0.9.0", backupSuffix: v090BackupSuffix,
		validate: validateV090Schema,
		exportMetadata: func(ctx context.Context, source *sql.Tx, dst io.Writer) error {
			return exportV090MetadataSnapshot(ctx, source, dst)
		},
		restorePhysical: restoreV090PhysicalCatalog,
	},
}

var (
	renameUpgradeFile         = os.Rename
	removeInvalidUpgradeStage = removeUpgradeFileSet
)

// prepareReleasedSchemaUpgrade recognizes only storage layouts that shipped in
// a public release. Older layouts rebuild through the same deterministic JSONL
// authority used by backup and restore; released schemas are never mutated in
// place. v0.9.0 is the sole unversioned released layout. Every later layout
// carries an explicit schema version and adds a source adapter here only when
// its physical or logical shape differs from the current schema.
func prepareReleasedSchemaUpgrade(path string, driver docsqlite.Driver) error {
	if err := validateReleasedStorageSchemas(); err != nil {
		return err
	}
	if err := recoverInterruptedUpgrade(path, driver); err != nil {
		return fmt.Errorf("recovering interrupted database upgrade: %w", err)
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
	switch {
	case kind.fresh, kind.current:
		return cleanupStaleUpgradeFiles(path)
	case kind.source != nil:
		return cutoverReleasedDatabase(path, driver, *kind.source)
	default:
		return errors.New("opening database: unsupported schema")
	}
}

func validateReleasedStorageSchemas() error {
	versions := make(map[int]bool, len(releasedStorageSchemas))
	suffixes := make(map[string]bool, len(releasedStorageSchemas))
	for _, source := range releasedStorageSchemas {
		if source.version < 1 || source.version >= currentStorageSchemaVersion {
			return fmt.Errorf("invalid released storage schema version %d", source.version)
		}
		if versions[source.version] {
			return fmt.Errorf("duplicate released storage schema version %d", source.version)
		}
		if source.release == "" || source.backupSuffix == "" ||
			source.validate == nil || source.exportMetadata == nil || source.restorePhysical == nil {
			return fmt.Errorf("released storage schema version %d is incomplete", source.version)
		}
		if suffixes[source.backupSuffix] {
			return fmt.Errorf("duplicate released storage backup suffix %q", source.backupSuffix)
		}
		versions[source.version] = true
		suffixes[source.backupSuffix] = true
	}
	for version := 1; version < currentStorageSchemaVersion; version++ {
		if !versions[version] {
			return fmt.Errorf("released storage schema version %d adapter is missing", version)
		}
	}
	return nil
}

func classifyDatabaseSchema(db *sql.DB) (databaseSchema, error) {
	blobs, err := tableColumns(db, "blobs")
	if err != nil {
		return databaseSchema{}, err
	}
	if len(blobs) == 0 {
		return databaseSchema{fresh: true}, nil
	}
	packs, err := tableColumns(db, "blob_packs")
	if err != nil {
		return databaseSchema{}, err
	}
	vaultMetadata, err := tableColumns(db, "vault_metadata")
	if err != nil {
		return databaseSchema{}, err
	}
	if slices.Contains(vaultMetadata, "schema_version") {
		var version int
		if err := db.QueryRow(`
			SELECT schema_version FROM vault_metadata WHERE singleton = 1`).Scan(&version); err != nil {
			return databaseSchema{}, fmt.Errorf("reading storage schema version: %w", err)
		}
		if version > currentStorageSchemaVersion {
			return databaseSchema{}, fmt.Errorf(
				"opening database: schema version %d is newer than binary schema %d; use a newer docbank binary",
				version, currentStorageSchemaVersion,
			)
		}
		if version == currentStorageSchemaVersion {
			if err := validateCurrentSchemaColumns(blobs, packs, vaultMetadata); err != nil {
				return databaseSchema{}, err
			}
			return databaseSchema{version: version, current: true}, nil
		}
		for i := range releasedStorageSchemas {
			source := &releasedStorageSchemas[i]
			if source.version != version {
				continue
			}
			if err := source.validate(db, blobs, packs); err != nil {
				return databaseSchema{}, err
			}
			return databaseSchema{version: version, source: source}, nil
		}
		return databaseSchema{}, fmt.Errorf(
			"opening database: schema version %d has no supported JSONL cutover",
			version,
		)
	}

	// v0.9.0 predates the explicit version marker. Match its complete released
	// fingerprint once; future released layouts must never rely on inference.
	for i := range releasedStorageSchemas {
		source := &releasedStorageSchemas[i]
		if source.version != 1 {
			continue
		}
		if err := source.validate(db, blobs, packs); err == nil {
			return databaseSchema{version: source.version, source: source}, nil
		}
	}
	return databaseSchema{}, fmt.Errorf(
		"opening database: unsupported schema: unversioned layout (blobs=%s blob_packs=%s)",
		strings.Join(blobs, ","), strings.Join(packs, ","),
	)
}

func validateCurrentSchemaColumns(blobs, packs, vaultMetadata []string) error {
	currentBlobColumns := []string{
		metadataCreatedAtField, "hash", "loose_encoding", "loose_stored_size", "pack_eligible", "size",
	}
	currentPackColumns := []string{
		metadataCreatedAtField, "entry_count", "live_entries", "live_raw_bytes", "live_stored_bytes",
		"max_live_raw_len", "max_live_stored_len", "pack_id", "scan_hash", "stored_bytes",
	}
	currentVaultColumns := []string{"schema_version", "singleton", "vault_uid"}
	if !slices.Equal(blobs, currentBlobColumns) || !slices.Equal(packs, currentPackColumns) ||
		!slices.Equal(vaultMetadata, currentVaultColumns) {
		return fmt.Errorf(
			"opening database: schema version %d has an unexpected layout (blobs=%s blob_packs=%s vault_metadata=%s)",
			currentStorageSchemaVersion,
			strings.Join(blobs, ","), strings.Join(packs, ","), strings.Join(vaultMetadata, ","),
		)
	}
	return nil
}

func validateV090Schema(db *sql.DB, blobs, packs []string) error {
	v090BlobColumns := []string{metadataCreatedAtField, "hash", "size"}
	v090PackColumns := []string{metadataCreatedAtField, "entry_count", "pack_id", "stored_bytes"}
	if !slices.Equal(blobs, v090BlobColumns) || !slices.Equal(packs, v090PackColumns) {
		return errors.New("not the released v0.9.0 schema")
	}
	return requireV090Tables(db)
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

func cutoverReleasedDatabase(
	path string, driver docsqlite.Driver, sourceSchema releasedStorageSchema,
) (err error) {
	backupPath := path + sourceSchema.backupSuffix
	stagePath := upgradeStagePath(path, sourceSchema.version)
	jsonlPath := upgradeJSONLPath(path, sourceSchema.version)
	if _, statErr := os.Stat(backupPath); statErr == nil {
		return fmt.Errorf(
			"upgrading %s database: recovery copy already exists at %s",
			sourceSchema.release, backupPath,
		)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("checking %s recovery copy: %w", sourceSchema.release, statErr)
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

	source, err := openReleasedSource(path, driver, sourceSchema)
	if err != nil {
		return err
	}
	snapshot, err := source.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		_ = source.Close()
		return fmt.Errorf("pinning %s metadata: %w", sourceSchema.release, err)
	}
	closeSource := func() error {
		return errors.Join(snapshot.Rollback(), source.Close())
	}

	if err := writeUpgradeJSONL(snapshot, jsonlPath, sourceSchema); err != nil {
		_ = closeSource()
		return err
	}
	target, err := openCurrentStore(stagePath, driver)
	if err != nil {
		_ = closeSource()
		return fmt.Errorf("creating current database for %s upgrade: %w", sourceSchema.release, err)
	}
	if err := importUpgradeJSONL(target, jsonlPath, sourceSchema); err != nil {
		_ = target.Close()
		_ = closeSource()
		return err
	}
	if err := sourceSchema.restorePhysical(context.Background(), snapshot, target); err != nil {
		_ = target.Close()
		_ = closeSource()
		return err
	}
	if err := target.ValidateMetadata(context.Background()); err != nil {
		_ = target.Close()
		_ = closeSource()
		return fmt.Errorf("validating upgraded %s metadata: %w", sourceSchema.release, err)
	}
	if err := target.Checkpoint(context.Background()); err != nil {
		_ = target.Close()
		_ = closeSource()
		return fmt.Errorf("checkpointing upgraded %s database: %w", sourceSchema.release, err)
	}
	if err := target.Close(); err != nil {
		_ = closeSource()
		return fmt.Errorf("closing upgraded %s database: %w", sourceSchema.release, err)
	}
	if err := closeSource(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return fmt.Errorf("closing %s source database: %w", sourceSchema.release, err)
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
		return fmt.Errorf("syncing %s upgrade staging: %w", sourceSchema.release, err)
	}
	if err := publishReleasedUpgrade(path, stagePath, backupPath); err != nil {
		return err
	}
	published = true
	return nil
}

func openReleasedSource(
	path string, driver docsqlite.Driver, sourceSchema releasedStorageSchema,
) (*sql.DB, error) {
	db, err := driver.Open(path, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Deferred,
	})
	if err != nil {
		return nil, fmt.Errorf("opening %s source database: %w", sourceSchema.release, err)
	}
	var busy, logFrames, checkpointed int64
	if err := db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(
		&busy, &logFrames, &checkpointed,
	); err != nil || busy != 0 || logFrames != 0 {
		_ = db.Close()
		if err != nil {
			return nil, fmt.Errorf("checkpointing %s source database: %w", sourceSchema.release, err)
		}
		return nil, fmt.Errorf(
			"checkpointing %s source database: WAL remains busy=%d log=%d checkpointed=%d",
			sourceSchema.release, busy, logFrames, checkpointed,
		)
	}
	return db, nil
}

func writeUpgradeJSONL(
	snapshot *sql.Tx, path string, sourceSchema releasedStorageSchema,
) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("creating %s upgrade metadata: %w", sourceSchema.release, err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	if err := sourceSchema.exportMetadata(context.Background(), snapshot, f); err != nil {
		return fmt.Errorf("exporting %s metadata: %w", sourceSchema.release, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing %s upgrade metadata: %w", sourceSchema.release, err)
	}
	return nil
}

func importUpgradeJSONL(target *Store, path string, sourceSchema releasedStorageSchema) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening %s upgrade metadata: %w", sourceSchema.release, err)
	}
	if err := target.ImportMetadata(context.Background(), f); err != nil {
		_ = f.Close()
		return fmt.Errorf("importing %s metadata: %w", sourceSchema.release, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing %s upgrade metadata: %w", sourceSchema.release, err)
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

func publishReleasedUpgrade(path, stagePath, backupPath string) error {
	if err := removeUpgradeFileSetSidecars(path); err != nil {
		return err
	}
	if err := syncRegularFile(path); err != nil {
		return err
	}
	if err := renameUpgradeFile(path, backupPath); err != nil {
		return fmt.Errorf("retaining released source recovery database: %w", err)
	}
	if err := syncUpgradeDirectory(path); err != nil {
		_ = renameUpgradeFile(backupPath, path)
		return fmt.Errorf("syncing released source recovery database: %w", err)
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

func recoverInterruptedUpgrade(path string, driver docsqlite.Driver) error {
	_, pathErr := os.Stat(path)
	if pathErr == nil {
		return nil
	}
	if !errors.Is(pathErr, os.ErrNotExist) {
		return pathErr
	}
	type interruptedUpgrade struct {
		backup string
		stage  string
	}
	var interrupted *interruptedUpgrade
	newestBackupVersion := 0
	newestBackupPath := ""
	for _, source := range releasedStorageSchemas {
		backupPath := path + source.backupSuffix
		stagePath := upgradeStagePath(path, source.version)
		_, backupErr := os.Stat(backupPath)
		_, stageErr := os.Stat(stagePath)
		if backupErr != nil && !errors.Is(backupErr, os.ErrNotExist) {
			return backupErr
		}
		if stageErr != nil && !errors.Is(stageErr, os.ErrNotExist) {
			return stageErr
		}
		if backupErr == nil && source.version > newestBackupVersion {
			newestBackupVersion = source.version
			newestBackupPath = backupPath
		}
		if stageErr == nil {
			if errors.Is(backupErr, os.ErrNotExist) {
				return fmt.Errorf("upgrade staging %s has no matching source recovery copy", stagePath)
			}
			if interrupted != nil {
				return errors.New("multiple interrupted database upgrades require operator inspection")
			}
			interrupted = &interruptedUpgrade{backup: backupPath, stage: stagePath}
		}
	}
	if interrupted != nil {
		backupPath := interrupted.backup
		stagePath := interrupted.stage
		if err := validateUpgradeStage(stagePath, driver); err != nil {
			if renameErr := renameUpgradeFile(backupPath, path); renameErr != nil {
				return errors.Join(err, renameErr)
			}
			if syncErr := syncUpgradeDirectory(path); syncErr != nil {
				return errors.Join(err, syncErr)
			}
			if cleanupErr := removeInvalidUpgradeStage(stagePath); cleanupErr != nil {
				return errors.Join(err, cleanupErr)
			}
			return syncUpgradeDirectory(path)
		}
		if err := renameUpgradeFile(stagePath, path); err != nil {
			return err
		}
		return syncUpgradeDirectory(path)
	}
	if newestBackupPath != "" {
		return fmt.Errorf(
			"database is missing while released recovery copy %s remains; refusing to resurrect an old vault without upgrade staging",
			newestBackupPath,
		)
	}
	return nil
}

func upgradeStagePath(path string, version int) string {
	return fmt.Sprintf("%s.upgrade-v%d-staging", path, version)
}

func upgradeJSONLPath(path string, version int) string {
	return fmt.Sprintf("%s.upgrade-v%d-metadata.jsonl", path, version)
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
	if !kind.current {
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
	var cleanupErr error
	for _, source := range releasedStorageSchemas {
		cleanupErr = errors.Join(
			cleanupErr,
			removeUpgradeFileSet(upgradeStagePath(path, source.version)),
			removeIfExists(upgradeJSONLPath(path, source.version)),
		)
	}
	return cleanupErr
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
	// Windows requires a write-capable handle for FlushFileBuffers, which backs
	// os.File.Sync. Both database owners are closed before this point.
	f, err := os.OpenFile(path, os.O_RDWR, 0)
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
