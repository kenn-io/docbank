package store

import (
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"

	docsqlite "go.kenn.io/docbank/sqlite"
	"go.kenn.io/docbank/sqlite/modernc"
)

//go:embed testdata/schema-v0.9.0.sql
var schemaV090SQL string

//go:embed testdata/schema-v0.10.0-physical.sql
var schemaV0100PhysicalSQL string

//go:embed testdata/schema-v0.11.0-addition.sql
var schemaV0110AdditionSQL string

//go:embed testdata/schema-v0.14.0.sql
var schemaV0140SQL string

type v090Fixture struct {
	looseHash  string
	packedHash string
	packID     string
	deadPackID string
	metadata   []byte
}

type v2Fixture struct {
	rawHash     string
	zstdHash    string
	packedHash  string
	missingHash string
	packID      string
	metadata    []byte
}

type v3Fixture struct {
	blobHash         string
	primaryStoreID   string
	secondaryStoreID string
	metadata         []byte
}

func TestOpenCutsOverReleasedV090ThroughJSONL(t *testing.T) {
	for _, test := range v090UpgradeDrivers() {
		t.Run(test.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "docbank.db")
			fixture := createV090Fixture(t, dbPath, test.driver)

			s, err := Open(dbPath, test.driver)
			require.NoError(t, err)
			var schemaVersion int
			require.NoError(t, s.db.QueryRow(`
				SELECT schema_version FROM vault_metadata WHERE singleton = 1`).Scan(&schemaVersion))
			assert.Equal(t, currentStorageSchemaVersion, schemaVersion)
			var upgraded bytes.Buffer
			require.NoError(t, s.ExportMetadata(t.Context(), &upgraded))
			assert.Equal(t, fixture.metadata, upgraded.Bytes(),
				"the released logical authority survives byte-for-byte")

			loose, err := s.PhysicalContent(t.Context(), fixture.looseHash)
			require.NoError(t, err)
			assert.Equal(t, PhysicalContent{
				Kind: "loose", Encoding: "raw", LogicalBytes: 5, StoredBytes: 5,
				PackEligible: true,
			}, loose)
			packed, err := s.PhysicalContent(t.Context(), fixture.packedHash)
			require.NoError(t, err)
			assert.Equal(t, "packed", packed.Kind)
			var restoredPackID string
			require.NoError(t, s.db.QueryRow(`
				SELECT pack_id FROM blob_pack_entries WHERE blob_hash = ?`,
				fixture.packedHash).Scan(&restoredPackID))
			assert.Equal(t, fixture.packID, restoredPackID)
			var deadLiveEntries int64
			require.NoError(t, s.db.QueryRow(`
				SELECT live_entries FROM blob_packs WHERE pack_id = ?`,
				fixture.deadPackID).Scan(&deadLiveEntries))
			assert.Zero(t, deadLiveEntries, "dead v0.9.0 pack inventory is preserved")
			require.NoError(t, s.Close())

			backupPath := dbPath + v090BackupSuffix
			backup, err := test.driver.Open(backupPath, docsqlite.OpenOptions{
				Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Deferred,
			})
			require.NoError(t, err)
			columns, err := tableColumns(backup, "blobs")
			require.NoError(t, err)
			assert.Equal(t, []string{"created_at", "hash", "size"}, columns,
				"the retained recovery database stays in the released schema")
			require.NoError(t, backup.Close())

			reopened, err := Open(dbPath, test.driver)
			require.NoError(t, err)
			require.NoError(t, reopened.Close())
		})
	}
}

func TestFreshStoresRecordCurrentStorageSchemaVersion(t *testing.T) {
	for _, test := range v090UpgradeDrivers() {
		t.Run(test.name, func(t *testing.T) {
			s, err := Open(filepath.Join(t.TempDir(), "docbank.db"), test.driver)
			require.NoError(t, err)
			defer func() { require.NoError(t, s.Close()) }()
			var version int
			require.NoError(t, s.db.QueryRow(`
				SELECT schema_version FROM vault_metadata WHERE singleton = 1`).Scan(&version))
			assert.Equal(t, currentStorageSchemaVersion, version)
		})
	}
}

func TestOpenCutsOverEveryReleasedSchemaV2LayoutThroughJSONL(t *testing.T) {
	layouts := []struct {
		name     string
		addition string
	}{
		{name: "v0.10.0"},
		{name: "v0.10.1-v0.11.0", addition: schemaV0110AdditionSQL},
	}
	for _, driver := range v090UpgradeDrivers() {
		for _, layout := range layouts {
			t.Run(driver.name+"/"+layout.name, func(t *testing.T) {
				dbPath := filepath.Join(t.TempDir(), "docbank.db")
				fixture := createV2Fixture(t, dbPath, driver.driver, layout.addition)

				s, err := Open(dbPath, driver.driver)
				require.NoError(t, err)
				var upgraded bytes.Buffer
				require.NoError(t, s.ExportMetadata(t.Context(), &upgraded))
				assert.Equal(t, fixture.metadata, upgraded.Bytes(),
					"released logical authority survives byte-for-byte")

				primary, err := s.PrimaryBlobStore(t.Context())
				require.NoError(t, err)
				assert.Equal(t, "filesystem", primary.Kind)
				assert.Equal(t, "primary", primary.Role)
				assert.NotEqual(t, "10000000-0000-4000-8000-000000000001", primary.ID)
				require.NoError(t, validateUUIDv4(primary.ID))
				require.NoError(t, validateUUIDv4(primary.OwnershipEpoch))

				assertPhysicalContent(t, s, fixture.rawHash, PhysicalContent{
					Kind: "loose", Encoding: "raw", LogicalBytes: 5, StoredBytes: 5,
					PackEligible: true,
				})
				assertPhysicalContent(t, s, fixture.zstdHash, PhysicalContent{
					Kind: "loose", Encoding: "zstd", LogicalBytes: 9, StoredBytes: 6,
					PackEligible: false,
				})
				packed, err := s.PhysicalContent(t.Context(), fixture.packedHash)
				require.NoError(t, err)
				assert.Equal(t, "packed", packed.Kind)
				_, err = s.PhysicalContent(t.Context(), fixture.missingHash)
				require.ErrorIs(t, err, ErrPhysicalAuthorityMissing)

				var storeID string
				require.NoError(t, s.db.QueryRow(`
					SELECT store_id FROM blob_pack_entries WHERE blob_hash = ?`,
					fixture.packedHash).Scan(&storeID))
				assert.Equal(t, primary.ID, storeID)
				require.NoError(t, s.Close())

				backupPath := dbPath + v2BackupSuffix
				backup, err := driver.driver.Open(backupPath, docsqlite.OpenOptions{
					Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Deferred,
				})
				require.NoError(t, err)
				kind, err := classifyDatabaseSchema(backup)
				require.NoError(t, err)
				assert.Equal(t, 2, kind.version)
				assert.NotNil(t, kind.source)
				require.NoError(t, backup.Close())
			})
		}
	}
}

func TestOpenCutsOverReleasedSchemaV3ThroughJSONL(t *testing.T) {
	for _, test := range v090UpgradeDrivers() {
		t.Run(test.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "docbank.db")
			fixture := createV3Fixture(t, dbPath, test.driver)

			s, err := Open(dbPath, test.driver)
			require.NoError(t, err)
			var schemaVersion int
			require.NoError(t, s.db.QueryRow(`
				SELECT schema_version FROM vault_metadata WHERE singleton=1`).Scan(&schemaVersion))
			assert.Equal(t, 4, schemaVersion)
			var upgraded bytes.Buffer
			require.NoError(t, s.ExportMetadata(t.Context(), &upgraded))
			assert.Equal(t, fixture.metadata, upgraded.Bytes())
			var stores, locations int
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM blob_stores
				WHERE store_id IN (?,?)`, fixture.primaryStoreID, fixture.secondaryStoreID).Scan(&stores))
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM blob_locations
				WHERE blob_hash=? AND store_id IN (?,?)`, fixture.blobHash,
				fixture.primaryStoreID, fixture.secondaryStoreID).Scan(&locations))
			assert.Equal(t, 2, stores)
			assert.Equal(t, 2, locations)
			require.NoError(t, s.Close())

			backup, err := test.driver.Open(dbPath+v3BackupSuffix, docsqlite.OpenOptions{
				Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Deferred,
			})
			require.NoError(t, err)
			kind, err := classifyDatabaseSchema(backup)
			require.NoError(t, err)
			assert.Equal(t, 3, kind.version)
			assert.NotNil(t, kind.source)
			require.NoError(t, backup.Close())
		})
	}
}

func TestOpenRejectsDatabaseFromNewerStorageSchema(t *testing.T) {
	for _, test := range v090UpgradeDrivers() {
		t.Run(test.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "docbank.db")
			s, err := Open(dbPath, test.driver)
			require.NoError(t, err)
			require.NoError(t, s.Close())

			db, err := test.driver.Open(dbPath, docsqlite.OpenOptions{
				Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate,
			})
			require.NoError(t, err)
			_, err = db.Exec(`UPDATE vault_metadata SET schema_version = ? WHERE singleton = 1`,
				currentStorageSchemaVersion+1)
			require.NoError(t, err)
			require.NoError(t, db.Close())

			_, err = Open(dbPath, test.driver)
			require.ErrorContains(t, err, "is newer than binary schema")
		})
	}
}

func TestCurrentSchemaFencesReleasedV090Binary(t *testing.T) {
	for _, test := range v090UpgradeDrivers() {
		t.Run(test.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "docbank.db")
			s, err := Open(dbPath, test.driver)
			require.NoError(t, err)
			require.NoError(t, s.Close())

			db, err := test.driver.Open(dbPath, docsqlite.OpenOptions{
				Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate,
			})
			require.NoError(t, err)
			_, err = db.Exec(schemaV090SQL)
			require.NoError(t, err, "the released bootstrap applies its idempotent schema first")
			var vaultID string
			err = db.QueryRow(`SELECT vault_id FROM vault_metadata WHERE singleton = 1`).Scan(&vaultID)
			require.ErrorContains(t, err, "no such column", "the released mandatory startup read must fail")
			require.NoError(t, db.Close())
		})
	}
}

func TestReleasedRecoveryCopyDoesNotResurrectDeletedVault(t *testing.T) {
	driver := DefaultSQLiteDriver()
	dbPath := filepath.Join(t.TempDir(), "docbank.db")
	createV090Fixture(t, dbPath, driver)
	s, err := Open(dbPath, driver)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	require.NoError(t, os.Remove(dbPath))

	_, err = Open(dbPath, driver)
	require.ErrorContains(t, err, "refusing to resurrect an old vault")
	_, statErr := os.Stat(dbPath)
	require.ErrorIs(t, statErr, os.ErrNotExist)
	_, statErr = os.Stat(dbPath + v090BackupSuffix)
	require.NoError(t, statErr)
}

func TestOpenCompletesInterruptedReleasedCutover(t *testing.T) {
	driver := DefaultSQLiteDriver()
	dbPath := filepath.Join(t.TempDir(), "docbank.db")
	fixture := createV090Fixture(t, dbPath, driver)
	sourceSchema := releasedStorageSchemas[0]
	stagePath := upgradeStagePath(dbPath, sourceSchema.version)
	jsonlPath := upgradeJSONLPath(dbPath, sourceSchema.version)

	source, err := openReleasedSource(dbPath, driver, sourceSchema)
	require.NoError(t, err)
	snapshot, err := source.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	require.NoError(t, writeUpgradeJSONL(snapshot, jsonlPath, sourceSchema))
	target, err := openCurrentStore(stagePath, driver)
	require.NoError(t, err)
	require.NoError(t, importUpgradeJSONL(target, jsonlPath, sourceSchema))
	require.NoError(t, sourceSchema.restorePhysical(t.Context(), snapshot, target))
	require.NoError(t, target.ValidateMetadata(t.Context()))
	require.NoError(t, target.Checkpoint(t.Context()))
	require.NoError(t, target.Close())
	require.NoError(t, snapshot.Rollback())
	require.NoError(t, source.Close())
	require.NoError(t, os.Remove(jsonlPath))
	require.NoError(t, os.Rename(dbPath, dbPath+sourceSchema.backupSuffix))

	recovered, err := Open(dbPath, driver)
	require.NoError(t, err)
	var metadata bytes.Buffer
	require.NoError(t, recovered.ExportMetadata(t.Context(), &metadata))
	assert.Equal(t, fixture.metadata, metadata.Bytes())
	require.NoError(t, recovered.Close())
	_, err = os.Stat(stagePath)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(dbPath + sourceSchema.backupSuffix)
	require.NoError(t, err)
}

func TestInvalidStageRestoresSourceBeforeRemovingRecoveryMarker(t *testing.T) {
	driver := DefaultSQLiteDriver()
	dbPath := filepath.Join(t.TempDir(), "docbank.db")
	createV090Fixture(t, dbPath, driver)
	sourceSchema := releasedStorageSchemas[0]
	stagePath := upgradeStagePath(dbPath, sourceSchema.version)
	require.NoError(t, os.WriteFile(stagePath, []byte("not a database"), 0o600))
	require.NoError(t, os.Rename(dbPath, dbPath+sourceSchema.backupSuffix))

	originalRemove := removeInvalidUpgradeStage
	t.Cleanup(func() { removeInvalidUpgradeStage = originalRemove })
	removeInvalidUpgradeStage = func(string) error {
		return errors.New("injected invalid-stage cleanup failure")
	}
	_, err := Open(dbPath, driver)
	require.ErrorContains(t, err, "injected invalid-stage cleanup failure")
	_, err = os.Stat(dbPath)
	require.NoError(t, err, "the released source is authoritative before marker cleanup")
	_, err = os.Stat(stagePath)
	require.NoError(t, err, "the failed cleanup leaves its interrupted-upgrade marker")
	_, err = os.Stat(dbPath + sourceSchema.backupSuffix)
	require.ErrorIs(t, err, os.ErrNotExist)

	removeInvalidUpgradeStage = originalRemove
	recovered, err := Open(dbPath, driver)
	require.NoError(t, err)
	require.NoError(t, recovered.Close())
}

func TestV090CutoverPublicationFailureRestoresReleasedDatabase(t *testing.T) {
	driver := DefaultSQLiteDriver()
	dbPath := filepath.Join(t.TempDir(), "docbank.db")
	createV090Fixture(t, dbPath, driver)
	originalRename := renameUpgradeFile
	t.Cleanup(func() { renameUpgradeFile = originalRename })
	calls := 0
	renameUpgradeFile = func(oldPath, newPath string) error {
		calls++
		if calls == 2 {
			return errors.New("injected upgraded-database publication failure")
		}
		return os.Rename(oldPath, newPath)
	}

	_, err := Open(dbPath, driver)
	require.ErrorContains(t, err, "injected upgraded-database publication failure")
	db, err := driver.Open(dbPath, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Deferred,
	})
	require.NoError(t, err)
	kind, err := classifyDatabaseSchema(db)
	require.NoError(t, err)
	assert.Equal(t, 1, kind.version, "the released source is restored after publication fails")
	assert.NotNil(t, kind.source)
	require.NoError(t, db.Close())
	_, err = os.Stat(dbPath + v090BackupSuffix)
	require.ErrorIs(t, err, os.ErrNotExist)

	renameUpgradeFile = originalRename
	s, err := Open(dbPath, driver)
	require.NoError(t, err)
	require.NoError(t, s.Close())
}

func createV090Fixture(t *testing.T, path string, driver docsqlite.Driver) v090Fixture {
	t.Helper()
	db, err := driver.Open(path, docsqlite.OpenOptions{
		Access: docsqlite.Create, TransactionMode: docsqlite.Immediate,
	})
	require.NoError(t, err)
	_, err = db.Exec(schemaV090SQL)
	require.NoError(t, err)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.Exec(`PRAGMA defer_foreign_keys = ON`)
	require.NoError(t, err)
	const (
		timestamp    = "2026-07-19T12:00:00.000000000Z"
		vaultID      = "10000000-0000-4000-8000-000000000001"
		looseHash    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		packedHash   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		danglingHash = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
		looseVer     = "20000000-0000-4000-8000-000000000001"
		packedVer    = "20000000-0000-4000-8000-000000000002"
		looseOp      = "30000000-0000-4000-8000-000000000001"
		packedOp     = "30000000-0000-4000-8000-000000000002"
	)
	packID := pack.NewPackID()
	deadPackID := pack.NewPackID()
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO vault_metadata(singleton, vault_id) VALUES(1, ?)`, []any{vaultID}},
		{`INSERT INTO blobs(hash, size, created_at) VALUES(?, 5, ?)`, []any{looseHash, timestamp}},
		{`INSERT INTO blobs(hash, size, created_at) VALUES(?, 7, ?)`, []any{packedHash, timestamp}},
		{`INSERT INTO nodes(id, parent_id, name, kind, current_version_id, revision,
			created_at, modified_at) VALUES(1, NULL, '', 'dir', NULL, 1, ?, ?)`,
			[]any{timestamp, timestamp}},
		{`INSERT INTO nodes(id, parent_id, name, kind, current_version_id, revision,
			created_at, modified_at) VALUES(2, 1, 'loose.txt', 'file', ?, 1, ?, ?)`,
			[]any{looseVer, timestamp, timestamp}},
		{`INSERT INTO nodes(id, parent_id, name, kind, current_version_id, revision,
			created_at, modified_at) VALUES(3, 1, 'packed.bin', 'file', ?, 1, ?, ?)`,
			[]any{packedVer, timestamp, timestamp}},
		{`INSERT INTO content_versions(version_id, node_id, blob_hash, size, mime_type,
			recorded_at, node_revision, introduced_operation_id, transition_kind)
			VALUES(?, 2, ?, 5, 'text/plain', ?, 1, ?, 'content_create')`,
			[]any{looseVer, looseHash, timestamp, looseOp}},
		{`INSERT INTO content_versions(version_id, node_id, blob_hash, size, mime_type,
			recorded_at, node_revision, introduced_operation_id, transition_kind)
			VALUES(?, 3, ?, 7, 'application/octet-stream', ?, 1, ?, 'content_create')`,
			[]any{packedVer, packedHash, timestamp, packedOp}},
		{`INSERT INTO blob_packs(pack_id, entry_count, stored_bytes, created_at)
			VALUES(?, 1, 7, ?)`, []any{packID, timestamp}},
		{`INSERT INTO blob_packs(pack_id, entry_count, stored_bytes, created_at)
			VALUES(?, 1, 9, ?)`, []any{deadPackID, timestamp}},
		{`INSERT INTO blob_pack_index(blob_hash, pack_id, pack_offset, stored_len,
			raw_len, flags, crc32c) VALUES(?, ?, ?, 7, 7, 0, 0)`,
			[]any{packedHash, packID, pack.MinEntryOffset}},
		{`INSERT INTO blob_pack_index(blob_hash, pack_id, pack_offset, stored_len,
			raw_len, flags, crc32c) VALUES(?, ?, ?, 9, 9, 0, 0)`,
			[]any{danglingHash, deadPackID, pack.MinEntryOffset}},
	}
	for _, statement := range statements {
		_, err := tx.Exec(statement.query, statement.args...)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())

	snapshot, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	var metadata bytes.Buffer
	require.NoError(t, exportV090MetadataSnapshot(t.Context(), snapshot, &metadata))
	require.NoError(t, snapshot.Rollback())
	require.NoError(t, db.Close())
	return v090Fixture{
		looseHash: looseHash, packedHash: packedHash, packID: packID,
		deadPackID: deadPackID, metadata: metadata.Bytes(),
	}
}

func createV2Fixture(
	t *testing.T, path string, driver docsqlite.Driver, layoutAddition string,
) v2Fixture {
	t.Helper()
	db, err := driver.Open(path, docsqlite.OpenOptions{
		Access: docsqlite.Create, TransactionMode: docsqlite.Immediate,
	})
	require.NoError(t, err)
	_, err = db.Exec(schemaV090SQL)
	require.NoError(t, err)
	_, err = db.Exec(schemaV0100PhysicalSQL)
	require.NoError(t, err)
	if layoutAddition != "" {
		_, err = db.Exec(layoutAddition)
		require.NoError(t, err)
	}
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.Exec(`PRAGMA defer_foreign_keys = ON`)
	require.NoError(t, err)
	const (
		timestamp   = "2026-07-19T12:00:00.000000000Z"
		vaultID     = "10000000-0000-4000-8000-000000000001"
		rawHash     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		zstdHash    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		packedHash  = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
		missingHash = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	)
	packID := pack.NewPackID()
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO vault_metadata(singleton, vault_uid, schema_version) VALUES(1, ?, 2)`,
			[]any{vaultID}},
		{`INSERT INTO nodes(id, parent_id, name, kind, current_version_id, revision,
			created_at, modified_at) VALUES(1, NULL, '', 'dir', NULL, 1, ?, ?)`,
			[]any{timestamp, timestamp}},
		{`INSERT INTO blobs(hash, size, created_at, loose_encoding, loose_stored_size,
			pack_eligible) VALUES(?, 5, ?, 'raw', 5, 1)`, []any{rawHash, timestamp}},
		{`INSERT INTO blobs(hash, size, created_at, loose_encoding, loose_stored_size,
			pack_eligible) VALUES(?, 9, ?, 'zstd', 6, 0)`, []any{zstdHash, timestamp}},
		{`INSERT INTO blobs(hash, size, created_at, pack_eligible)
			VALUES(?, 7, ?, 1)`, []any{packedHash, timestamp}},
		{`INSERT INTO blobs(hash, size, created_at, pack_eligible)
			VALUES(?, 11, ?, 1)`, []any{missingHash, timestamp}},
		{`INSERT INTO blob_packs(pack_id, entry_count, stored_bytes, created_at)
			VALUES(?, 1, 7, ?)`, []any{packID, timestamp}},
		{`INSERT INTO blob_pack_index(blob_hash, pack_id, pack_offset, stored_len,
			raw_len, flags, crc32c) VALUES(?, ?, ?, 7, 7, 0, 0)`,
			[]any{packedHash, packID, pack.MinEntryOffset}},
	}
	for _, statement := range statements {
		_, err := tx.Exec(statement.query, statement.args...)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())

	snapshot, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	var metadata bytes.Buffer
	require.NoError(t, exportMetadataSnapshot(t.Context(), snapshot, &metadata))
	require.NoError(t, snapshot.Rollback())
	require.NoError(t, db.Close())
	return v2Fixture{
		rawHash: rawHash, zstdHash: zstdHash, packedHash: packedHash,
		missingHash: missingHash, packID: packID, metadata: metadata.Bytes(),
	}
}

func createV3Fixture(t *testing.T, path string, driver docsqlite.Driver) v3Fixture {
	t.Helper()
	db, err := driver.Open(path, docsqlite.OpenOptions{
		Access: docsqlite.Create, TransactionMode: docsqlite.Immediate,
	})
	require.NoError(t, err)
	_, err = db.Exec(schemaV0140SQL)
	require.NoError(t, err)
	const (
		timestamp        = "2026-08-16T12:00:00.000000000Z"
		vaultID          = "10000000-0000-4000-8000-000000000003"
		blobHash         = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		primaryStoreID   = "20000000-0000-4000-8000-000000000003"
		secondaryStoreID = "20000000-0000-4000-8000-000000000004"
	)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.Exec(`PRAGMA defer_foreign_keys=ON`)
	require.NoError(t, err)
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO vault_metadata(singleton,vault_uid,schema_version) VALUES(1,?,3)`, []any{vaultID}},
		{`INSERT INTO nodes(id,parent_id,name,kind,revision,created_at,modified_at)
			VALUES(1,NULL,'','dir',1,?,?)`, []any{timestamp, timestamp}},
		{`INSERT INTO blobs(hash,size,created_at) VALUES(?,17,?)`, []any{blobHash, timestamp}},
		{`INSERT INTO blob_stores(store_id,name,kind,role,lifecycle,binding,ownership_epoch,created_at)
			VALUES(?,'primary','filesystem','primary','managed','{}',?,?)`,
			[]any{primaryStoreID, "30000000-0000-4000-8000-000000000003", timestamp}},
		{`INSERT INTO blob_stores(store_id,name,kind,role,lifecycle,binding,ownership_epoch,created_at)
			VALUES(?,'archive','filesystem','archive','managed','{}',?,?)`,
			[]any{secondaryStoreID, "30000000-0000-4000-8000-000000000004", timestamp}},
		{`INSERT INTO blob_locations(blob_hash,store_id,generation,kind,encoding,stored_size,pack_eligible)
			VALUES(?,?,'1','loose','raw',17,1)`, []any{blobHash, primaryStoreID}},
		{`INSERT INTO blob_locations(blob_hash,store_id,generation,kind,encoding,stored_size,pack_eligible)
			VALUES(?,?,'2','loose','raw',17,0)`, []any{blobHash, secondaryStoreID}},
	}
	for _, statement := range statements {
		_, err = tx.Exec(statement.query, statement.args...)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	snapshot, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	var metadata bytes.Buffer
	require.NoError(t, exportMetadataSnapshot(t.Context(), snapshot, &metadata))
	require.NoError(t, snapshot.Rollback())
	require.NoError(t, db.Close())
	return v3Fixture{
		blobHash: blobHash, primaryStoreID: primaryStoreID,
		secondaryStoreID: secondaryStoreID, metadata: metadata.Bytes(),
	}
}

func assertPhysicalContent(t *testing.T, s *Store, hash string, want PhysicalContent) {
	t.Helper()
	got, err := s.PhysicalContent(t.Context(), hash)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func v090UpgradeDrivers() []struct {
	name   string
	driver docsqlite.Driver
} {
	drivers := []docsqlite.Driver{DefaultSQLiteDriver(), modernc.Driver{}}
	seen := make(map[string]bool)
	result := make([]struct {
		name   string
		driver docsqlite.Driver
	}, 0, len(drivers))
	for _, driver := range drivers {
		if seen[driver.Name()] {
			continue
		}
		seen[driver.Name()] = true
		result = append(result, struct {
			name   string
			driver docsqlite.Driver
		}{name: driver.Name(), driver: driver})
	}
	return result
}
