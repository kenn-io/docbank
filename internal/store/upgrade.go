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

	docsqlite "go.kenn.io/docbank/sqlite"
)

const (
	v090BackupSuffix = ".v0.9.0.bak"
	v2BackupSuffix   = ".schema-v2.bak"
	v3BackupSuffix   = ".schema-v3.bak"
)

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
	{
		version: 2, release: "schema v2 (v0.10.0–v0.11.0)", backupSuffix: v2BackupSuffix,
		validate: validateV2Schema,
		exportMetadata: func(ctx context.Context, source *sql.Tx, dst io.Writer) error {
			return exportMetadataSnapshot(ctx, source, dst)
		},
		restorePhysical: restoreV2PhysicalCatalog,
	},
	{
		version: 3, release: "schema v3 (v0.12.0–v0.14.0)", backupSuffix: v3BackupSuffix,
		validate: validateV3Schema,
		exportMetadata: func(ctx context.Context, source *sql.Tx, dst io.Writer) error {
			return exportMetadataSnapshot(ctx, source, dst)
		},
		restorePhysical: restoreV3PhysicalCatalog,
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
			if err := validateCurrentSchemaColumns(db, blobs, packs, vaultMetadata); err != nil {
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

func validateCurrentSchemaColumns(
	db *sql.DB,
	blobs, packs, vaultMetadata []string,
) error {
	currentBlobColumns := []string{
		metadataCreatedAtField, "hash", metadataSizeField,
	}
	currentPackColumns := []string{
		metadataCreatedAtField, "entry_count", "live_entries", "live_raw_bytes", "live_stored_bytes",
		"max_live_raw_len", "max_live_stored_len", "pack_id", "scan_hash", "store_id", "stored_bytes",
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
	wantTables := map[string][]string{
		"blob_stores": {
			"binding", metadataCreatedAtField, "kind", "lifecycle", "name",
			"ownership_epoch", "role", "store_id",
		},
		"blob_locations": {
			"blob_hash", "encoding", "generation", "kind", "pack_eligible",
			"store_id", "stored_size",
		},
		"blob_pack_entries": {
			"blob_hash", "crc32c", "flags", "pack_id", "pack_offset",
			"raw_len", "store_id", "stored_len",
		},
	}
	for table, want := range wantTables {
		got, err := tableColumns(db, table)
		if err != nil {
			return err
		}
		if !slices.Equal(got, want) {
			return fmt.Errorf(
				"opening database: schema version %d has an unexpected %s layout (%s)",
				currentStorageSchemaVersion, table, strings.Join(got, ","),
			)
		}
	}
	return nil
}

func validateV2Schema(db *sql.DB, blobs, packs []string) error {
	v2BlobColumns := []string{
		metadataCreatedAtField, "hash", "loose_encoding", "loose_stored_size",
		"pack_eligible", metadataSizeField,
	}
	v2PackColumns := []string{
		metadataCreatedAtField, "entry_count", "live_entries", "live_raw_bytes",
		"live_stored_bytes", "max_live_raw_len", "max_live_stored_len", "pack_id",
		"scan_hash", "stored_bytes",
	}
	if !slices.Equal(blobs, v2BlobColumns) || !slices.Equal(packs, v2PackColumns) {
		return errors.New("not a released schema-v2 database")
	}
	index, err := tableColumns(db, "blob_pack_index")
	if err != nil {
		return err
	}
	wantIndex := []string{
		"blob_hash", "crc32c", "flags", "pack_id", "pack_offset", "raw_len", "stored_len",
	}
	if !slices.Equal(index, wantIndex) {
		return errors.New("not a released schema-v2 database")
	}
	return requireV090Tables(db)
}

func validateV3Schema(db *sql.DB, blobs, packs []string) error {
	v3BlobColumns := []string{metadataCreatedAtField, "hash", metadataSizeField}
	v3PackColumns := []string{
		metadataCreatedAtField, "entry_count", "live_entries", "live_raw_bytes",
		"live_stored_bytes", "max_live_raw_len", "max_live_stored_len", "pack_id",
		"scan_hash", "store_id", "stored_bytes",
	}
	if !slices.Equal(blobs, v3BlobColumns) || !slices.Equal(packs, v3PackColumns) {
		return errors.New("not a released schema-v3 database")
	}
	required := []string{
		"audit_authority", "audit_baselines", "audit_memberships", "audit_records", "audit_scopes",
		"blob_locations", "blob_pack_entries", "blob_packs", "blob_stores", "blobs",
		"content_versions", "extracted_text", "ingests", "node_tags", "nodes", "provenance",
		"storage_operation_cleanup", "storage_operation_stores", "storage_operations", "tags",
		"text_extraction_queue", "text_searchable_versions", "vault_metadata", "watch_sources",
	}
	for _, table := range required {
		columns, err := tableColumns(db, table)
		if err != nil {
			return err
		}
		if len(columns) == 0 {
			return fmt.Errorf("not a released schema-v3 database: missing table %s", table)
		}
	}
	for _, table := range []string{"processing_profiles", "rendition_builds", "rendition_attachments"} {
		columns, err := tableColumns(db, table)
		if err != nil {
			return err
		}
		if len(columns) != 0 {
			return fmt.Errorf("not a released schema-v3 database: unexpected table %s", table)
		}
	}
	return nil
}

func validateV090Schema(db *sql.DB, blobs, packs []string) error {
	v090BlobColumns := []string{metadataCreatedAtField, "hash", metadataSizeField}
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
	if _, err := target.MigrateLegacyPlainText(context.Background()); err != nil {
		_ = target.Close()
		_ = closeSource()
		return fmt.Errorf("migrating %s legacy plain-text authority: %w", sourceSchema.release, err)
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
		if err := resetImportedPhysicalCatalog(ctx, tx); err != nil {
			return err
		}
		if err := restoreV090LooseLocations(
			ctx, source, tx, target.primaryStoreID,
		); err != nil {
			return err
		}
		packIDs, err := restoreReleasedPackRecords(
			ctx, source, tx, target.primaryStoreID, "v0.9.0",
		)
		if err != nil {
			return err
		}
		if err := restoreReleasedPackMappings(
			ctx, source, tx, target.primaryStoreID, "v0.9.0",
		); err != nil {
			return err
		}
		for _, packID := range packIDs {
			if err := finalizePackScanHash(
				ctx, tx, target.primaryStoreID, packID,
			); err != nil {
				return err
			}
		}
		return nil
	})
}

func restoreV2PhysicalCatalog(ctx context.Context, source metadataQuerier, target *Store) error {
	return target.withStorageTx(ctx, func(tx *sql.Tx) error {
		if err := resetImportedPhysicalCatalog(ctx, tx); err != nil {
			return err
		}
		if err := restoreV2LooseLocations(
			ctx, source, tx, target.primaryStoreID,
		); err != nil {
			return err
		}
		packIDs, err := restoreReleasedPackRecords(
			ctx, source, tx, target.primaryStoreID, "schema-v2",
		)
		if err != nil {
			return err
		}
		if err := restoreReleasedPackMappings(
			ctx, source, tx, target.primaryStoreID, "schema-v2",
		); err != nil {
			return err
		}
		for _, packID := range packIDs {
			if err := finalizePackScanHash(
				ctx, tx, target.primaryStoreID, packID,
			); err != nil {
				return err
			}
		}
		return nil
	})
}

func restoreV3PhysicalCatalog(ctx context.Context, source metadataQuerier, target *Store) error {
	return target.withStorageTx(ctx, func(tx *sql.Tx) error {
		if err := resetImportedPhysicalCatalog(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM blob_stores`); err != nil {
			return fmt.Errorf("resetting imported blob stores: %w", err)
		}
		primaryStoreID, err := restoreV3BlobStores(ctx, source, tx)
		if err != nil {
			return err
		}
		if err := restoreV3BlobLocations(ctx, source, tx); err != nil {
			return err
		}
		if err := restoreV3BlobPacks(ctx, source, tx); err != nil {
			return err
		}
		if err := restoreV3BlobPackEntries(ctx, source, tx); err != nil {
			return err
		}
		target.primaryStoreID = primaryStoreID
		return nil
	})
}

func restoreV3BlobStores(ctx context.Context, source metadataQuerier, tx *sql.Tx) (string, error) {
	rows, err := source.QueryContext(ctx, `
		SELECT store_id,name,kind,role,lifecycle,binding,ownership_epoch,created_at
		FROM blob_stores ORDER BY store_id`)
	if err != nil {
		return "", fmt.Errorf("reading schema-v3 blob stores: %w", err)
	}
	defer func() { _ = rows.Close() }()
	primaryStoreID := ""
	for rows.Next() {
		var storeID, name, kind, role, lifecycle, binding, ownershipEpoch, createdAt string
		if err := rows.Scan(&storeID, &name, &kind, &role, &lifecycle,
			&binding, &ownershipEpoch, &createdAt); err != nil {
			return "", fmt.Errorf("scanning schema-v3 blob store: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO blob_stores(
			store_id,name,kind,role,lifecycle,binding,ownership_epoch,created_at
		) VALUES(?,?,?,?,?,?,?,?)`, storeID, name, kind, role, lifecycle,
			binding, ownershipEpoch, createdAt); err != nil {
			return "", fmt.Errorf("restoring schema-v3 blob store %s: %w", storeID, err)
		}
		if role == blobStoreRolePrimary {
			primaryStoreID = storeID
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("reading schema-v3 blob stores: %w", err)
	}
	if primaryStoreID == "" {
		return "", errors.New("schema-v3 physical catalog has no primary blob store")
	}
	return primaryStoreID, nil
}

func restoreV3BlobLocations(ctx context.Context, source metadataQuerier, tx *sql.Tx) error {
	rows, err := source.QueryContext(ctx, `
		SELECT blob_hash,store_id,generation,kind,encoding,stored_size,pack_eligible
		FROM blob_locations ORDER BY blob_hash,store_id`)
	if err != nil {
		return fmt.Errorf("reading schema-v3 blob locations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var blobHash, storeID, generation, kind string
		var encoding sql.NullString
		var storedSize int64
		var packEligible bool
		if err := rows.Scan(&blobHash, &storeID, &generation, &kind, &encoding,
			&storedSize, &packEligible); err != nil {
			return fmt.Errorf("scanning schema-v3 blob location: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO blob_locations(
			blob_hash,store_id,generation,kind,encoding,stored_size,pack_eligible
		) VALUES(?,?,?,?,?,?,?)`, blobHash, storeID, generation, kind, encoding,
			storedSize, packEligible); err != nil {
			return fmt.Errorf("restoring schema-v3 blob location %s/%s: %w", blobHash, storeID, err)
		}
	}
	return rows.Err()
}

func restoreV3BlobPacks(ctx context.Context, source metadataQuerier, tx *sql.Tx) error {
	rows, err := source.QueryContext(ctx, `SELECT
		store_id,pack_id,entry_count,stored_bytes,created_at,scan_hash,
		live_entries,live_stored_bytes,live_raw_bytes,max_live_stored_len,max_live_raw_len
		FROM blob_packs ORDER BY store_id,pack_id`)
	if err != nil {
		return fmt.Errorf("reading schema-v3 blob packs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var storeID, packID, createdAt, scanHash string
		var entryCount, storedBytes, liveEntries, liveStoredBytes int64
		var liveRawBytes, maxLiveStoredLen, maxLiveRawLen int64
		if err := rows.Scan(&storeID, &packID, &entryCount, &storedBytes, &createdAt,
			&scanHash, &liveEntries, &liveStoredBytes, &liveRawBytes,
			&maxLiveStoredLen, &maxLiveRawLen); err != nil {
			return fmt.Errorf("scanning schema-v3 blob pack: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO blob_packs(
			store_id,pack_id,entry_count,stored_bytes,created_at,scan_hash,
			live_entries,live_stored_bytes,live_raw_bytes,max_live_stored_len,max_live_raw_len
		) VALUES(?,?,?,?,?,?,0,0,0,0,0)`, storeID, packID, entryCount, storedBytes,
			createdAt, scanHash); err != nil {
			return fmt.Errorf("restoring schema-v3 blob pack %s/%s: %w", storeID, packID, err)
		}
	}
	return rows.Err()
}

func restoreV3BlobPackEntries(ctx context.Context, source metadataQuerier, tx *sql.Tx) error {
	rows, err := source.QueryContext(ctx, `SELECT
		blob_hash,store_id,pack_id,pack_offset,stored_len,raw_len,flags,crc32c
		FROM blob_pack_entries ORDER BY blob_hash,store_id`)
	if err != nil {
		return fmt.Errorf("reading schema-v3 blob pack entries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var blobHash, storeID, packID string
		var packOffset, storedLen, rawLen, flags, crc32c int64
		if err := rows.Scan(&blobHash, &storeID, &packID, &packOffset,
			&storedLen, &rawLen, &flags, &crc32c); err != nil {
			return fmt.Errorf("scanning schema-v3 blob pack entry: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO blob_pack_entries(
			blob_hash,store_id,pack_id,pack_offset,stored_len,raw_len,flags,crc32c
		) VALUES(?,?,?,?,?,?,?,?)`, blobHash, storeID, packID, packOffset,
			storedLen, rawLen, flags, crc32c); err != nil {
			return fmt.Errorf("restoring schema-v3 blob pack entry %s/%s: %w", blobHash, storeID, err)
		}
	}
	return rows.Err()
}

func resetImportedPhysicalCatalog(ctx context.Context, tx *sql.Tx) error {
	for _, table := range []string{"blob_pack_entries", "blob_packs", "blob_locations"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table); err != nil {
			return fmt.Errorf("resetting imported %s: %w", table, err)
		}
	}
	return nil
}

func restoreV090LooseLocations(
	ctx context.Context,
	source metadataQuerier,
	tx *sql.Tx,
	storeID string,
) error {
	rows, err := source.QueryContext(ctx, `
		SELECT b.hash, b.size
		FROM blobs b
		WHERE NOT EXISTS (
			SELECT 1 FROM blob_pack_index i WHERE i.blob_hash = b.hash
		)
		ORDER BY b.hash`)
	if err != nil {
		return fmt.Errorf("reading v0.9.0 loose authority: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var hash string
		var size int64
		if err := rows.Scan(&hash, &size); err != nil {
			return fmt.Errorf("scanning v0.9.0 loose authority: %w", err)
		}
		if err := writeLooseLocationTx(ctx, tx, storeID, hash, BlobPhysical{
			Encoding: looseEncodingRaw, StoredBytes: size,
			PackEligible: size <= maxPackEligibleBytes,
		}); err != nil {
			return err
		}
	}
	return rows.Err()
}

func restoreV2LooseLocations(
	ctx context.Context,
	source metadataQuerier,
	tx *sql.Tx,
	storeID string,
) error {
	rows, err := source.QueryContext(ctx, `
		SELECT hash, size, loose_encoding, loose_stored_size, pack_eligible
		FROM blobs WHERE loose_encoding IS NOT NULL ORDER BY hash`)
	if err != nil {
		return fmt.Errorf("reading schema-v2 loose authority: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var hash, encoding string
		var size, stored int64
		var eligible bool
		if err := rows.Scan(&hash, &size, &encoding, &stored, &eligible); err != nil {
			return fmt.Errorf("scanning schema-v2 loose authority: %w", err)
		}
		if err := writeLooseLocationTx(ctx, tx, storeID, hash, BlobPhysical{
			Encoding: encoding, StoredBytes: stored, PackEligible: eligible,
		}); err != nil {
			return err
		}
	}
	return rows.Err()
}

func restoreReleasedPackRecords(
	ctx context.Context,
	source metadataQuerier,
	tx *sql.Tx,
	storeID string,
	release string,
) ([]string, error) {
	rows, err := source.QueryContext(ctx, `
		SELECT pack_id, entry_count, stored_bytes, created_at
		FROM blob_packs ORDER BY created_at, pack_id`)
	if err != nil {
		return nil, fmt.Errorf("reading %s pack records: %w", release, err)
	}
	defer func() { _ = rows.Close() }()
	var packIDs []string
	for rows.Next() {
		record, err := scanPackRecord(rows)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO blob_packs(store_id, pack_id, entry_count, stored_bytes, created_at)
			VALUES(?, ?, ?, ?, ?)`,
			storeID, record.PackID, record.EntryCount, record.StoredBytes,
			record.CreatedAt.UTC().Format(timestampLayout)); err != nil {
			return nil, fmt.Errorf("restoring %s pack %s: %w", release, record.PackID, err)
		}
		packIDs = append(packIDs, record.PackID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading %s pack records: %w", release, err)
	}
	return packIDs, nil
}

func restoreReleasedPackMappings(
	ctx context.Context,
	source metadataQuerier,
	tx *sql.Tx,
	storeID string,
	release string,
) error {
	rows, err := source.QueryContext(ctx, `
		SELECT blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c
		FROM blob_pack_index ORDER BY blob_hash`)
	if err != nil {
		return fmt.Errorf("reading %s pack mappings: %w", release, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		entry, err := scanPackEntry(rows)
		if err != nil {
			return err
		}
		var member bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM blobs WHERE hash=?)`,
			entry.Hash.String(),
		).Scan(&member); err != nil {
			return fmt.Errorf(
				"checking %s packed blob %s membership: %w",
				release, entry.Hash, err,
			)
		}
		if !member {
			// Released layouts permitted stale pack mappings after logical
			// membership disappeared. The immutable pack record remains useful
			// dead-byte inventory, but the mapping cannot become authority in
			// the rebuilt catalog.
			continue
		}
		if err := writeAdoption(ctx, tx, storeID, entry, false); err != nil {
			return fmt.Errorf("restoring %s packed blob %s: %w", release, entry.Hash, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading %s pack mappings: %w", release, err)
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
	if _, err := store.MigrateLegacyPlainText(context.Background()); err != nil {
		return errors.Join(
			fmt.Errorf("migrating interrupted upgrade staging authority: %w", err),
			store.Close(),
		)
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
