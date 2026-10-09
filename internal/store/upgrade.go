package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/pack"

	docsqlite "go.kenn.io/docbank/sqlite"
)

const (
	v090BackupSuffix = ".v0.9.0.bak"
	v2BackupSuffix   = ".schema-v2.bak"
	v3BackupSuffix   = ".schema-v3.bak"
	v28BackupSuffix  = ".schema-v28.bak"
)

// releasedStorageSchemaVersions lists the storage schema of every public
// release. Each version older than the current schema needs a cutover adapter.
// Add a release's version here when it ships; scripts/check-released-schemas.sh
// fails CI when a release tag's schema is missing.
var releasedStorageSchemaVersions = []int{1, 2, 3, 28, 29}

// Metadata JSONL does not carry unfinished storage work. Cutovers copy these
// tables verbatim so queued imports, placements, evacuations, and pending
// deletions survive an upgrade.
var (
	releasedStorageOperationTables = []string{
		"storage_operations", "storage_operation_stores", "storage_operation_cleanup",
	}
	releasedPendingDeletionTables = []string{
		"gc_loose_retirements", "derivative_blob_purge_pending", "derivative_pack_purge_pending",
	}
)

type databaseSchema struct {
	version int
	fresh   bool
	current bool
	source  *releasedStorageSchema
}

type releasedStorageSchema struct {
	version        int
	release        string
	backupSuffix   string
	validate       func(*sql.DB, []string, []string) error
	exportMetadata func(context.Context, *sql.Tx, io.Writer) error
	// restoreSourceState copies source state that metadata JSONL does not
	// carry, such as the physical blob catalog, after the import.
	restoreSourceState func(context.Context, metadataQuerier, *Store) error
	// keepsProcessingIncarnation makes the target adopt the source's current
	// processing incarnation, so existing consent stays valid.
	keepsProcessingIncarnation bool
}

var releasedStorageSchemas = []releasedStorageSchema{
	{
		version: 1, release: "v0.9.0", backupSuffix: v090BackupSuffix,
		validate: validateV090Schema,
		exportMetadata: func(ctx context.Context, source *sql.Tx, dst io.Writer) error {
			return exportReleasedMetadataSnapshot(ctx, source, dst, 1)
		},
		restoreSourceState: restoreV090PhysicalCatalog,
	},
	{
		version: 2, release: "schema v2 (v0.10.0–v0.11.0)", backupSuffix: v2BackupSuffix,
		validate: validateV2Schema,
		exportMetadata: func(ctx context.Context, source *sql.Tx, dst io.Writer) error {
			return exportReleasedMetadataSnapshot(ctx, source, dst, 2)
		},
		restoreSourceState: restoreV2PhysicalCatalog,
	},
	{
		version: 3, release: "schema v3 (v0.12.0–v0.14.0)", backupSuffix: v3BackupSuffix,
		validate: validateV3Schema,
		exportMetadata: func(ctx context.Context, source *sql.Tx, dst io.Writer) error {
			return exportReleasedMetadataSnapshot(ctx, source, dst, 3)
		},
		restoreSourceState: restoreV3PhysicalCatalog,
	},
	{
		version: 28, release: "v0.15.0", backupSuffix: v28BackupSuffix,
		validate: func(db *sql.DB, _, _ []string) error {
			return validateReleasedLayout(db, "v0.15.0", schemaV28Layout)
		},
		exportMetadata: func(ctx context.Context, source *sql.Tx, dst io.Writer) error {
			return exportReleasedMetadataSnapshot(ctx, source, dst, 28)
		},
		restoreSourceState:         restoreV28SourceState,
		keepsProcessingIncarnation: true,
	},
}

var (
	renameUpgradeFile         = atomicfile.Replace
	removeInvalidUpgradeStage = removeUpgradeFileSet
)

var currentSchemaTables = [...]string{
	"blobs", "blob_packs", "vault_metadata",
	"mailbox_containers", "mailbox_chunks",
	"mailbox_archives", "mailbox_transfer_receipts",
	"mailbox_jobs", "mailbox_occurrences",
	"email_document_publications", "email_document_relations",
	"photo_assets", "photo_files", "photo_library_settings", "photo_sets", "photo_set_members", "photo_change_receipts",
	"photo_technical_metadata", "photo_technical_metadata_state",
	"email_generations", "email_part_artifacts", "email_attachments", "email_heads", "email_body_results",
	"blob_stores", "blob_locations", "blob_pack_entries",
	"saved_queries", "saved_query_runs", "collection_labels", "provenance_version_bindings", "batch_tag_receipts", "package_preflights",
	"term_report_history",
	"export_sources", "export_chunks", "export_members", "export_plans", "export_documents", "export_role_roots", "export_jobs",
	"collection_snapshots", "collection_snapshot_members", "collection_snapshot_representations", "packages", "package_volumes",
	"package_records", "package_labels", "package_import_jobs", "package_import_receipts",
	"bates_namespaces", "bates_namespace_cursors", "bates_allocations", "bates_page_labels",
	"bates_artifacts", "bates_artifact_pages",
	"page_documents", "page_frames", "page_recipes", "page_images", "page_render_jobs",
	"document_event_state", "document_event_generations", "document_event_heads",
	"document_event_builds", "document_event_dirty", "document_event_attempts",
	"document_events", "document_event_actors", "document_event_primaries",
	"vector_index_generations", "vector_index_heads", "vector_index_build_jobs",
	"vector_index_reader_leases", "vector_index_unavailable_coverage",
	"media_sources", "media_source_versions", "media_occurrences", "media_input_artifacts",
	"media_operations",
	"processing_consent_grants", "rendition_job_waiters", "embedding_jobs",
	"persons", "person_identities", "person_external_identities", "person_external_uid_aliases",
	"person_aliases", "person_merges", "person_splits", "custodian_assignments",
	"document_people_state", "person_match_candidates", "person_document_assertions",
	"document_people_generations", "document_people_heads", "document_people_builds",
	"document_people",
}

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
	kind, classifyErr := classifyDatabaseSchema(driver, db)
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
			source.validate == nil || source.exportMetadata == nil || source.restoreSourceState == nil {
			return fmt.Errorf("released storage schema version %d is incomplete", source.version)
		}
		if suffixes[source.backupSuffix] {
			return fmt.Errorf("duplicate released storage backup suffix %q", source.backupSuffix)
		}
		versions[source.version] = true
		suffixes[source.backupSuffix] = true
	}
	for _, version := range releasedStorageSchemaVersions {
		if version < currentStorageSchemaVersion && !versions[version] {
			return fmt.Errorf("released storage schema version %d adapter is missing", version)
		}
		delete(versions, version)
	}
	for version := range versions {
		return fmt.Errorf("storage schema version %d has an adapter but no release", version)
	}
	return nil
}

func classifyDatabaseSchema(driver docsqlite.Driver, db *sql.DB) (databaseSchema, error) {
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
			if err := validateCurrentSchemaColumns(driver, db, blobs, packs, vaultMetadata); err != nil {
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
	driver docsqlite.Driver, db *sql.DB,
	blobs, packs, vaultMetadata []string,
) error {
	wantTables, err := canonicalCurrentSchemaColumns(driver)
	if err != nil {
		return fmt.Errorf("deriving current schema columns: %w", err)
	}
	if !slices.Equal(blobs, wantTables["blobs"]) || !slices.Equal(packs, wantTables["blob_packs"]) ||
		!slices.Equal(vaultMetadata, wantTables["vault_metadata"]) {
		return fmt.Errorf(
			"opening database: schema version %d has an unexpected layout (blobs=%s blob_packs=%s vault_metadata=%s)",
			currentStorageSchemaVersion,
			strings.Join(blobs, ","), strings.Join(packs, ","), strings.Join(vaultMetadata, ","),
		)
	}
	for _, table := range currentSchemaTables[3:] {
		got, err := tableColumns(db, table)
		if err != nil {
			return err
		}
		if !slices.Equal(got, wantTables[table]) {
			return fmt.Errorf(
				"opening database: schema version %d has an unexpected %s layout (%s)",
				currentStorageSchemaVersion, table, strings.Join(got, ","),
			)
		}
	}
	return validateEmbeddingCatalogSchema(context.Background(), db)
}

func canonicalCurrentSchemaColumns(driver docsqlite.Driver) (map[string][]string, error) {
	return deriveCurrentSchemaColumns(driver)
}

func deriveCurrentSchemaColumns(driver docsqlite.Driver) (columns map[string][]string, err error) {
	tempFile, err := os.CreateTemp("", "docbank-current-schema-*.db")
	if err != nil {
		return nil, fmt.Errorf("creating temporary database: %w", err)
	}
	tempPath := tempFile.Name()
	var db *sql.DB
	defer func() {
		var closeErr error
		if db != nil {
			closeErr = db.Close()
		}
		err = errors.Join(err, closeErr, os.Remove(tempPath))
	}()
	if err := tempFile.Close(); err != nil {
		return nil, fmt.Errorf("closing temporary database file: %w", err)
	}

	db, err = driver.Open(tempPath, docsqlite.OpenOptions{
		Access: docsqlite.Create, TransactionMode: docsqlite.Immediate,
	})
	if err != nil {
		return nil, fmt.Errorf("opening temporary database with %s: %w", driver.Name(), err)
	}
	// Match bootstrap's transaction boundary instead of syncing every schema
	// statement separately each time an existing vault is opened.
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("beginning temporary schema transaction: %w", err)
	}
	if _, err := tx.Exec(schemaSQL); err != nil {
		return nil, errors.Join(
			fmt.Errorf("applying current schema to temporary database: %w", err), tx.Rollback(),
		)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing temporary schema transaction: %w", err)
	}
	columns = make(map[string][]string, len(currentSchemaTables))
	for _, table := range &currentSchemaTables {
		columns[table], err = tableColumns(db, table)
		if err != nil {
			return nil, err
		}
		slices.Sort(columns[table])
	}
	return columns, nil
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
		columnBlobHash, "crc32c", "flags", "pack_id", "pack_offset", "raw_len", "stored_len",
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
	return queryTableColumns(context.Background(), db, table)
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
	var incarnation *metadataProcessingIncarnation
	if sourceSchema.keepsProcessingIncarnation {
		incarnation = &metadataProcessingIncarnation{Type: metadataProcessingIncarnationType}
		if err := snapshot.QueryRow(`SELECT i.incarnation_id,i.created_at FROM processing_incarnations i
			JOIN current_processing_incarnation c ON c.incarnation_id=i.incarnation_id WHERE c.singleton=1`,
		).Scan(&incarnation.ID, &incarnation.CreatedAt); err != nil {
			_ = closeSource()
			return fmt.Errorf("reading %s processing incarnation: %w", sourceSchema.release, err)
		}
	}
	target, err := openCurrentStore(stagePath, driver, incarnation)
	if err != nil {
		_ = closeSource()
		return fmt.Errorf("creating current database for %s upgrade: %w", sourceSchema.release, err)
	}
	if err := importUpgradeJSONL(target, jsonlPath, sourceSchema); err != nil {
		_ = target.Close()
		_ = closeSource()
		return err
	}
	if err := sourceSchema.restoreSourceState(context.Background(), snapshot, target); err != nil {
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
		return restoreV3PhysicalCatalogTx(ctx, source, tx, target)
	})
}

// restoreV28SourceState restores the v3-style blob catalog, copies unfinished
// storage work, and returns unfinished rendition jobs their authorization.
func restoreV28SourceState(ctx context.Context, source metadataQuerier, target *Store) error {
	return target.withStorageTx(ctx, func(tx *sql.Tx) error {
		if err := restoreV3PhysicalCatalogTx(ctx, source, tx, target); err != nil {
			return err
		}
		if err := copyReleasedTables(ctx, source, tx, releasedStorageOperationTables); err != nil {
			return err
		}
		if err := copyReleasedTables(ctx, source, tx, releasedPendingDeletionTables); err != nil {
			return err
		}
		return restoreRenditionJobAuthorization(ctx, source, tx)
	})
}

// restoreRenditionJobAuthorization puts back the authorization that metadata
// import clears from unfinished rendition jobs. A restore needs fresh consent,
// but an upgrade keeps the processing incarnation that authorized these jobs.
func restoreRenditionJobAuthorization(ctx context.Context, source metadataQuerier, tx *sql.Tx) error {
	rows, err := source.QueryContext(ctx, `SELECT job_id,selected_waiter_id,authorization_grant_id,
		authorization_incarnation_id,authorization_revocation_fence FROM rendition_jobs
		WHERE state IN (?,?,?) ORDER BY job_id`, RenditionJobQueued, RenditionJobRunning, RenditionJobRetryWait)
	if err != nil {
		return fmt.Errorf("reading released rendition job authorization: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var jobID string
		var waiterID, grantID, incarnationID sql.NullString
		var fence sql.NullInt64
		if err := rows.Scan(&jobID, &waiterID, &grantID, &incarnationID, &fence); err != nil {
			return fmt.Errorf("reading released rendition job authorization: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE rendition_jobs SET selected_waiter_id=?,
			authorization_grant_id=?,authorization_incarnation_id=?,authorization_revocation_fence=?
			WHERE job_id=?`, waiterID, grantID, incarnationID, fence, jobID); err != nil {
			return fmt.Errorf("restoring rendition job %s authorization: %w", jobID, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading released rendition job authorization: %w", err)
	}
	return rows.Close()
}

func copyReleasedTables(ctx context.Context, source metadataQuerier, tx *sql.Tx, tables []string) error {
	for _, table := range tables {
		if err := copyReleasedTable(ctx, source, tx, table); err != nil {
			return err
		}
	}
	return nil
}

func copyReleasedTable(ctx context.Context, source metadataQuerier, tx *sql.Tx, table string) error {
	sourceColumns, err := queryTableColumns(ctx, source, table)
	if err != nil {
		return err
	}
	targetColumns, err := queryTableColumns(ctx, tx, table)
	if err != nil {
		return err
	}
	if len(sourceColumns) == 0 || !slices.Equal(sourceColumns, targetColumns) {
		return fmt.Errorf("copying released %s: columns (%s) differ from current (%s)",
			table, strings.Join(sourceColumns, ","), strings.Join(targetColumns, ","))
	}
	columns := strings.Join(sourceColumns, ",")
	rows, err := source.QueryContext(ctx, `SELECT `+columns+` FROM `+table+` ORDER BY rowid`)
	if err != nil {
		return fmt.Errorf("reading released %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	insert := `INSERT INTO ` + table + `(` + columns + `) VALUES(?` + strings.Repeat(",?", len(sourceColumns)-1) + `)`
	values := make([]any, len(sourceColumns))
	pointers := make([]any, len(values))
	for i := range values {
		pointers[i] = &values[i]
	}
	for rows.Next() {
		if err := rows.Scan(pointers...); err != nil {
			return fmt.Errorf("reading released %s row: %w", table, err)
		}
		if _, err := tx.ExecContext(ctx, insert, values...); err != nil {
			return fmt.Errorf("copying released %s row: %w", table, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading released %s rows: %w", table, err)
	}
	return rows.Close()
}

func queryTableColumns(ctx context.Context, q metadataQuerier, table string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM pragma_table_info(?) ORDER BY name`, table)
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

func validateReleasedLayout(db *sql.DB, release, layout string) error {
	var columns map[string][]string
	if err := json.Unmarshal([]byte(layout), &columns); err != nil {
		return fmt.Errorf("reading released %s columns: %w", release, err)
	}
	for table, want := range columns {
		got, err := tableColumns(db, table)
		if err != nil {
			return err
		}
		if !slices.Equal(got, want) {
			return fmt.Errorf("not a released %s database: unexpected %s columns (%s)",
				release, table, strings.Join(got, ","))
		}
	}
	return nil
}

func restoreV3PhysicalCatalogTx(ctx context.Context, source metadataQuerier, tx *sql.Tx, target *Store) error {
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
	kind, classifyErr := classifyDatabaseSchema(driver, db)
	closeErr := db.Close()
	if classifyErr != nil || closeErr != nil {
		return errors.Join(classifyErr, closeErr)
	}
	if !kind.current {
		return errors.New("interrupted upgrade staging database does not use the current schema")
	}
	store, err := openCurrentStore(path, driver, nil)
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

// schemaV28Layout freezes every table column of the released v0.15.0 schema.
const schemaV28Layout = `{
  "audit_authority": ["allocation_entry_count", "allocation_genesis_digest", "allocation_head", "lineage_id", "operation_sequence_high_water", "singleton"],
  "audit_baselines": ["digest", "operation_id", "scope_id", "target_node_id"],
  "audit_memberships": ["baseline_digest", "node_id", "scope_id"],
  "audit_records": ["digest", "entry_count", "event_id", "event_ordinal", "kind", "node_id", "operation_id", "operation_sequence", "record_json", "scope_id"],
  "audit_scopes": ["chain_head", "enable_operation_id", "entry_count", "scope_id", "target_node_id"],
  "batch_tag_receipts": ["operation_id", "receipt_json", "request_digest"],
  "bates_allocations": ["allocation_id", "committed_at", "created_at", "end_sequence", "namespace_id", "operation_id", "recipe_sha256", "request_sha256", "snapshot_id", "start_sequence", "state"],
  "bates_artifact_pages": ["artifact_id", "label", "occurrence_id", "ordinal", "output_page", "source_blob_sha256", "source_page"],
  "bates_artifacts": ["allocation_id", "artifact_id", "blob_hash", "created_at", "manifest_sha256", "media_type", "page_count", "recipe_json", "size", "state"],
  "bates_namespace_cursors": ["namespace_id", "next_sequence"],
  "bates_namespaces": ["created_at", "namespace_id", "padding", "prefix", "suffix"],
  "bates_page_labels": ["allocation_id", "label", "namespace_id", "occurrence_id", "ordinal", "output_page", "sequence", "source_page"],
  "blob_checksums": ["blob_sha256", "md5"],
  "blob_locations": ["blob_hash", "encoding", "generation", "kind", "pack_eligible", "store_id", "stored_size"],
  "blob_pack_entries": ["blob_hash", "crc32c", "flags", "pack_id", "pack_offset", "raw_len", "store_id", "stored_len"],
  "blob_packs": ["created_at", "entry_count", "live_entries", "live_raw_bytes", "live_stored_bytes", "max_live_raw_len", "max_live_stored_len", "pack_id", "scan_hash", "store_id", "stored_bytes"],
  "blob_stores": ["binding", "created_at", "kind", "lifecycle", "name", "ownership_epoch", "role", "store_id"],
  "blobs": ["created_at", "hash", "size"],
  "collection_labels": ["ingest_id", "label", "revision", "updated_at"],
  "collection_snapshot_members": ["blob_sha256", "canonical_json", "checksum", "content_version_id", "display_name", "document_kind", "family_id", "family_order", "frozen_fields_json", "node_id", "occurrence_id", "ordinal", "parent_occurrence_id", "selected_pdf_sha256", "selected_source_pages_json", "size", "snapshot_id", "source_page_count"],
  "collection_snapshot_representations": ["blob_sha256", "canonical_json", "checksum", "content_version_id", "lexical_generation_id", "media_type", "occurrence_id", "ordinal", "page_number", "recipe_sha256", "rendition_build_id", "role", "size", "snapshot_id", "status", "text_authority", "verified_page_count"],
  "collection_snapshots": ["canonical_json", "checksum", "manifest_sha256", "member_count", "member_hash", "page_count", "predecessor_id", "sealed_at", "snapshot_id", "source_collection_ids_json", "vault_uid"],
  "content_versions": ["blob_hash", "introduced_operation_id", "mime_type", "node_id", "node_revision", "recorded_at", "size", "source_version_id", "transition_kind", "version_id"],
  "current_processing_incarnation": ["incarnation_id", "singleton"],
  "current_rendition_roots": ["active", "expires_at", "fencing_token", "recorded_at", "released_at", "root_id", "root_kind", "target_id", "target_kind"],
  "custodian_assignments": ["assignment_id", "basis", "content_version_id", "ingest_id", "node_id", "package_id", "package_record_id", "person_id", "rank", "raw_label", "raw_label_folded", "recorded_at", "retired_at", "revision", "scope_kind", "source_ref"],
  "derivative_blob_purge_pending": ["blob_hash"],
  "derivative_pack_purge_pending": ["pack_id", "store_id"],
  "derivative_purge_suppressions": ["active", "build_id", "profile_fingerprint", "purged_at", "source_sha256", "superseded_at", "superseding_build_id"],
  "document_event_actors": ["actor_key", "address", "claim_json", "display_name", "event_id", "evidence_id", "evidence_kind", "generation_id", "ordinal", "role", "sensitive"],
  "document_event_attempts": ["attempted_at", "content_version_id", "diagnostic_json", "input_epoch", "input_revision", "inputs_sha256", "state"],
  "document_event_builds": ["deriver_fingerprint", "failed", "finished_at", "operation_id", "published", "request_sha256", "scanned", "started_at", "state", "target_epoch", "unavailable", "updated_at"],
  "document_event_dirty": ["content_version_id", "reason", "revision"],
  "document_event_generations": ["canonical_json", "checksum", "content_version_id", "contract_version", "created_at", "deriver_fingerprint", "document_kind", "event_count", "generation_id", "inputs_sha256"],
  "document_event_heads": ["content_version_id", "generation_id", "input_epoch", "published_at"],
  "document_event_primaries": ["disclosure", "event_id", "generation_id", "reason", "rule_id", "scope_class"],
  "document_event_state": ["contract_version", "deriver_fingerprint", "input_epoch", "publication_epoch", "singleton", "updated_at"],
  "document_events": ["axis_key", "claim_basis", "date_kind", "date_value", "event_id", "evidence_id", "evidence_kind", "evidence_locator", "evidence_sha256", "fraction_digits", "generation_id", "offset_seconds", "parse_confidence", "precision", "raw_value", "sensitive", "source_key", "source_kind_raw", "timezone_kind", "utc_key", "zone_text"],
  "document_people": ["actor_key", "basis", "claim_count", "confidence", "content_version_id", "evidence_id", "evidence_kind", "first_axis_key", "generation_id", "last_axis_key", "node_id", "person_id", "raw_label", "role", "sensitive"],
  "document_people_builds": ["failed", "finished_at", "operation_id", "published", "request_sha256", "resolver_fingerprint", "scanned", "started_at", "state", "target_epoch", "updated_at"],
  "document_people_generations": ["canonical_json", "checksum", "content_version_id", "created_at", "generation_id", "inputs_sha256", "resolver_fingerprint"],
  "document_people_heads": ["binding_epoch", "candidate_overflow", "content_version_id", "edge_count", "event_generation_id", "failure_reason", "generation_id", "inputs_sha256", "node_revision", "published_at", "resolver_fingerprint", "state", "suppressed_actors", "unresolved_actors"],
  "document_people_state": ["binding_epoch", "publication_epoch", "resolver_fingerprint", "singleton", "updated_at"],
  "email_attachments": ["attached_at", "attachment_id", "content_version_id", "generation_id"],
  "email_body_results": ["body_recipe_fingerprint", "email_attachment_id", "part_path", "reason", "rendition_attachment_id", "state"],
  "email_document_publications": ["email_attachment_id", "operation_id", "parent_version_id", "receipt_json", "request_digest", "request_json"],
  "email_document_relations": ["child_version_id", "occurrence_order", "operation_id"],
  "email_generations": ["canonical_json", "checksum", "created_at", "generation_id", "recipe_fingerprint", "source_sha256", "source_size"],
  "email_heads": ["attachment_id", "content_version_id", "published_at"],
  "email_part_artifacts": ["blob_hash", "generation_id", "part_path", "role", "size"],
  "embedding_failures": ["attachment_id", "binding_id", "content_version_id", "failed_at", "failure_code", "fencing_token", "input_kind", "profile_fingerprint"],
  "embedding_generation_inputs": ["generation_id", "input_id", "input_order", "rendered_checksum"],
  "embedding_heads": ["binding_id", "content_version_id", "embedding_set_id", "fencing_token", "input_kind", "profile_fingerprint", "published_at", "vector_space_id"],
  "embedding_input_generations": ["attachment_context_fingerprint", "attachment_id", "chunk_policy_fingerprint", "created_at", "evidence_fingerprint", "formatter_fingerprint", "generation_blob_hash", "generation_checksum", "generation_encoded_size", "generation_id", "input_count", "profile_fingerprint", "source_version_id", "tokenizer_fingerprint"],
  "embedding_jobs": ["authorization_grant_id", "authorization_incarnation_id", "authorization_revocation_fence", "available_at", "binding_id", "claim_count", "claim_epoch", "claim_owner", "content_version_id", "created_at", "failure_code", "generation_id", "input_kind", "job_id", "lease_expires_at", "principal", "profile_fingerprint", "receipt_json", "scope", "state", "updated_at", "vault_uid", "vector_space_id"],
  "embedding_sets": ["binding_id", "content_version_id", "created_at", "embedding_input_fingerprint", "embedding_set_id", "input_generation_id", "input_kind", "profile_fingerprint", "vault_uid", "vector_set_id", "vector_space_id"],
  "embedding_vector_rows": ["checksum", "dimensions", "input_id", "row_id", "row_order", "vector_set_id"],
  "embedding_vector_sets": ["contract_version", "dimensions", "manifest_checksum", "payload_blob_hash", "payload_checksum", "payload_size", "row_count", "vector_set_id", "vector_space_id"],
  "embedding_vector_spaces": ["compatibility_id", "contract_version", "descriptor_fingerprint", "descriptor_json", "dimensions", "document_formatter", "metric", "model_input_fingerprint", "normalization", "provider_descriptor", "provider_revision", "query_formatter", "scalar_encoding", "vector_space_id"],
  "export_chunks": ["canonical_json", "chunk_index", "source_id"],
  "export_documents": ["canonical_json", "ordinal", "plan_id"],
  "export_jobs": ["archive_name", "canonical_json", "epoch", "expires_at", "id", "owner", "plan_id", "request_sha256", "state", "token"],
  "export_members": ["blob_hash", "canonical_json", "node_id", "source_id", "version_id"],
  "export_plans": ["canonical_json", "expires_at", "id", "owner", "request_sha256", "source_id"],
  "export_role_roots": ["blob_hash", "plan_id"],
  "export_sources": ["canonical_json", "expires_at", "id", "owner", "request_json", "request_sha256", "state"],
  "extracted_text": ["attempts", "blob_hash", "error", "extracted_at", "extractor", "extractor_version", "id", "status", "text"],
  "gc_loose_retirements": ["blob_hash", "loose_encoding", "store_id"],
  "ingests": ["id", "source_desc", "source_kind", "started_at"],
  "mailbox_archives": ["description", "id", "owner"],
  "mailbox_chunks": ["blob_hash", "chunk_index", "container_id", "size"],
  "mailbox_containers": ["created_at", "format", "id", "manifest_sha256", "owner", "sha256", "size", "state"],
  "mailbox_jobs": ["claim", "container_id", "id", "job_json", "owner", "state"],
  "mailbox_occurrences": ["job_id", "occurrence_json", "ordinal", "receipt_id"],
  "mailbox_transfer_receipts": ["archive_id", "document_publication_id", "id", "receipt_json", "source_ref", "target_version_id"],
  "media_input_artifacts": ["content_version_id", "created_at", "input_id", "input_sha256", "kind", "language", "occurrence_id", "origin", "provider", "source_id", "source_version_id"],
  "media_occurrences": ["caller_filename", "caller_occurrence_ref", "caller_person_ref", "caller_principal", "caller_revision", "first_seen_at", "message_json", "occurrence_id", "revoked_at", "source_id", "source_version_id", "speaker_label", "visible"],
  "media_operations": ["created_at", "operation_id", "principal", "receipt_json", "request_sha256", "source_id", "updated_at", "verb"],
  "media_source_versions": ["capture_json", "content_version_id", "created_at", "revision", "source_id", "source_version_id"],
  "media_sources": ["created_at", "identity_sha256", "kind", "origin_scope", "provider", "source_id"],
  "node_tags": ["node_id", "tag_id"],
  "nodes": ["created_at", "current_version_id", "id", "kind", "modified_at", "name", "parent_id", "revision", "trash_name", "trash_parent", "trashed_at"],
  "package_import_jobs": ["claim_owner", "created_at", "epoch", "id", "job_json", "lease_expires_at", "operation_id", "owner", "package_id", "preflight_id", "request_sha256", "state", "token", "updated_at"],
  "package_import_receipts": ["content_version_id", "occurrence_id", "package_id", "receipt_id", "receipt_json", "record_key", "recorded_at", "state"],
  "package_labels": ["artifact_id", "content_version_id", "endpoint", "label", "label_set", "occurrence_id", "package_id", "page_number", "page_state", "provenance"],
  "package_preflights": ["blocking", "canonical_json", "created_at", "diagnostics_blob_sha256", "diagnostics_json", "expires_at", "manifest_blob_sha256", "manifest_sha256", "mapping_json", "mapping_sha256", "owner", "preflight_id", "profile_json", "profile_sha256", "source_kind", "source_locator", "source_ref"],
  "package_records": ["load_file", "occurrence_id", "package_id", "raw_json", "raw_sha256", "row_id", "row_ordinal", "sensitive"],
  "package_volumes": ["declared_root", "mapped_root", "ordinal", "package_id", "resolved_root_sha256", "volume_name"],
  "packages": ["completed_at", "created_at", "direction", "export_plan_id", "ingest_id", "manifest_blob_sha256", "manifest_sha256", "mapping_json", "mapping_sha256", "package_id", "package_name", "party_label", "predecessor_package_id", "produced_on", "profile_json", "profile_sha256", "relation", "snapshot_id", "state"],
  "page_documents": ["canonical_json", "checksum", "version_id"],
  "page_frames": ["canonical_json", "checksum", "page", "version_id"],
  "page_images": ["blob_hash", "canonical_json", "checksum", "page", "recipe_sha256", "version_id"],
  "page_recipes": ["canonical_json", "checksum"],
  "page_render_jobs": ["created_at", "epoch", "failure_code", "id", "node_id", "request_json", "request_sha256", "results_json", "state", "token", "updated_at", "version_id"],
  "person_aliases": ["reason", "retired_at", "retired_person_id", "surviving_person_id"],
  "person_document_assertions": ["action", "assertion_id", "content_version_id", "note", "person_id", "recorded_at", "revision", "role"],
  "person_external_identities": ["archive_id", "display_name_snapshot", "last_seen_revision", "linked_at", "person_id", "system", "uid", "uid_kind", "uid_state", "updated_at"],
  "person_external_uid_aliases": ["archive_id", "observed_at", "retired_uid", "surviving_uid", "system"],
  "person_identities": ["confidence", "evidence_id", "evidence_kind", "identity_id", "kind", "normalization", "origin", "person_id", "recorded_at", "scope_kind", "scope_value", "value_display", "value_normalized"],
  "person_match_candidates": ["actor_key", "candidate_id", "created_at", "decided_at", "decided_person_id", "display_name", "evidence_json", "evidence_sha256", "occurrence_count", "reason", "revision", "state", "suggested_person_id"],
  "person_merges": ["absorbed_display_name", "absorbed_person_id", "created_at", "merge_id", "moved_json", "operation_id", "request_sha256", "survivor_person_id", "survivor_revision_after", "survivor_revision_before"],
  "person_splits": ["created_at", "operation_id", "receipt_json", "request_sha256"],
  "persons": ["created_at", "display_name", "display_name_folded", "origin", "person_id", "revision", "state", "updated_at"],
  "photo_assets": ["asset_id", "created_at", "display_file_id", "display_override_file_id", "excluded_at", "kind", "revision", "updated_at"],
  "photo_change_receipts": ["after_json", "after_revision", "asset_id", "before_json", "before_revision", "created_at", "operation", "receipt_id", "settings_key"],
  "photo_files": ["asset_id", "created_at", "file_id", "node_id", "role", "sidecar_of_file_id"],
  "photo_library_settings": ["preference", "revision", "singleton", "updated_at"],
  "photo_technical_metadata": ["camera_make", "camera_make_folded", "camera_model", "camera_model_folded", "capture_date", "capture_sort_key", "capture_time", "capture_time_offset", "capture_time_precision", "capture_time_raw", "capture_time_timezone", "exposure_bias_ev", "exposure_time_seconds", "f_number", "focal_length_mm", "generation_id", "height_px", "iso", "latitude", "lens_make", "lens_make_folded", "lens_model", "lens_model_folded", "location_label", "longitude", "orientation", "width_px"],
  "photo_technical_metadata_state": ["projection_recipe", "singleton"],
  "processing_consent_grants": ["consent_set_id", "disclosure_fingerprint", "expires_at", "grant_id", "incarnation_id", "input_classes_json", "issued_at", "principal", "profile_fingerprint", "retained_classes_json", "revocation_fence", "scope", "vault_uid"],
  "processing_consent_revocations": ["fence", "incarnation_id", "principal", "revocation_id", "revoked_at", "scope", "vault_uid"],
  "processing_incarnations": ["created_at", "incarnation_id"],
  "processing_profiles": ["attachment_policy_fingerprint", "canonical_profile", "consent_fingerprint", "evidence_lexical_fingerprint", "profile_fingerprint", "rendition_disclosure_fingerprint", "rendition_request_fingerprint", "retention_disclosure_fingerprint", "trust_boundary"],
  "provenance": ["identity", "ingest_id", "node_id", "original_mtime", "original_path", "supersedes"],
  "provenance_version_bindings": ["basis_ref", "content_version_id", "observed_at", "provenance_identity"],
  "rendition_artifacts": ["artifact_id", "blob_hash", "build_id", "checksum", "role", "size"],
  "rendition_attachments": ["attached_at", "attachment_id", "attachment_policy_fingerprint", "build_id", "consent_fingerprint", "content_version_id", "profile_fingerprint", "rendition_disclosure_fingerprint", "retention_disclosure_fingerprint", "trust_boundary", "vault_uid"],
  "rendition_blob_staging": ["blob_hash"],
  "rendition_builds": ["authorization_checksum", "build_id", "captured_artifact_policy_fingerprint", "captured_artifact_policy_json", "completed_at", "completeness", "declared_artifact_count", "evidence_checksum", "evidence_lexical_fingerprint", "lexical_segment_count", "markdown_checksum", "partial_success", "provider_operation_id", "provider_receipt_json", "rendition_checksum", "rendition_request_fingerprint", "source_sha256", "truncated", "unit_count", "vault_uid", "warnings_json"],
  "rendition_heads": ["attachment_id", "content_version_id", "profile_fingerprint", "published_at"],
  "rendition_job_waiters": ["attachment_id", "authorization_grant_id", "authorization_incarnation_id", "authorization_revocation_fence", "content_version_id", "created_at", "disclosure_fingerprint", "failure_code", "input_classes_json", "job_id", "principal", "profile_fingerprint", "retained_classes_json", "scope", "state", "updated_at", "waiter_id"],
  "rendition_jobs": ["authorization_grant_id", "authorization_incarnation_id", "authorization_revocation_fence", "available_at", "captured_artifact_policy_fingerprint", "captured_artifact_policy_json", "claim_epoch", "claim_owner", "created_at", "evidence_lexical_fingerprint", "execution_identity_fingerprint", "execution_identity_json", "execution_snapshot_json", "failure_code", "job_id", "lease_expires_at", "lexical_generation_id", "phase", "provider_attempts", "provider_resume_handle", "provider_started", "rendition_request_fingerprint", "selected_waiter_id", "source_sha256", "state", "updated_at", "vault_uid"],
  "rendition_lexical_generation_builds": ["build_id", "generation_id"],
  "rendition_lexical_generation_manifests": ["build_digest", "generation_id", "manifest_digest"],
  "rendition_lexical_generations": ["build_count", "built_at", "generation_id", "segment_count"],
  "rendition_lexical_heads": ["generation_id", "singleton"],
  "rendition_lexical_index": ["build_id", "row_id", "segment_id", "text"],
  "rendition_lexical_segments": ["build_id", "char_end", "char_start", "checksum", "segment_id", "segment_order", "text", "unit_id"],
  "rendition_lexical_superseded": ["generation_id"],
  "rendition_units": ["build_id", "checksum", "evidence_unit_id", "heading_path_json", "locator_json", "unit_id", "unit_order"],
  "saved_queries": ["created_at", "description", "fingerprint", "id", "kind", "name", "payload", "revision", "updated_at"],
  "saved_query_runs": ["expires_at", "member_hash", "previous_member_hash", "previous_query_fingerprint", "previous_run_id", "previous_total", "query_fingerprint", "ran_at", "run_id", "saved_query_id", "saved_query_revision", "snapshot_id", "total", "total_bytes"],
  "source_metadata_generations": ["canonical_json", "checksum", "contract_version", "created_at", "extractor_fingerprint", "generation_id", "source_sha256"],
  "source_metadata_heads": ["generation_id", "published_at", "source_sha256"],
  "storage_operation_cleanup": ["loose_encoding", "loose_hash", "operation_id", "pack_id", "store_id"],
  "storage_operation_stores": ["operation_id", "role", "store_id"],
  "storage_operations": ["cancel_requested", "completed_objects", "copied_bytes", "copied_objects", "created_at", "cursor", "error", "finished_at", "kind", "operation_id", "plan_json", "receipt_json", "request_digest", "request_json", "request_version", "retention_until", "source_store_id", "state", "total_objects", "updated_at"],
  "tags": ["id", "name", "revision"],
  "term_report_history": ["id", "observed_at", "parent_id", "request_json", "summary_json"],
  "text_extraction_queue": ["blob_hash", "next_attempt_at"],
  "text_searchable_versions": ["version_id"],
  "vault_metadata": ["schema_version", "singleton", "vault_uid"],
  "vector_index_build_jobs": ["fencing_token", "lease_expires_at", "owner", "source_manifest_checksum", "vector_space_id"],
  "vector_index_generations": ["built_at", "byte_size", "generation_bytes", "generation_id", "index_manifest_checksum", "row_count", "source_manifest_checksum", "vector_space_id"],
  "vector_index_heads": ["generation_id", "source_manifest_checksum", "vector_space_id"],
  "vector_index_reader_leases": ["fencing_token", "generation_id", "lease_expires_at", "lease_id", "owner"],
  "vector_index_unavailable_coverage": ["embedding_set_id", "external_reembedding_required", "payload_blob_hash", "source_manifest_checksum", "vector_set_id", "vector_space_id"],
  "visual_preview_generations": ["canonical_result", "checksum", "content_version_id", "contract_version", "created_at", "failure_code", "failure_detail", "generation_id", "output_blob_hash", "output_height", "output_media_type", "output_size", "output_width", "recipe_fingerprint", "source_sha256", "state", "vault_uid"],
  "visual_preview_heads": ["content_version_id", "generation_id", "published_at"],
  "watch_sources": ["blob_hash", "node_id", "size", "source_ref", "watch_name"]
}`
