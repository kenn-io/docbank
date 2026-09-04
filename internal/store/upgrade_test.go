package store

import (
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
			assertReleasedMetadataWithEmptyLexicalHead(t, fixture.metadata, upgraded.Bytes())

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

// Mutations caught: omitting legacy rendition migration from the released
// JSONL rebuild, deleting the recovery cache, or serving both legacy and
// rendition FTS after publication.
func TestUpgradeLegacyPlainTextCutsOverServingAuthority(t *testing.T) {
	for _, test := range v090UpgradeDrivers() {
		t.Run(test.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "docbank.db")
			fixture := createV090Fixture(t, dbPath, test.driver)
			const legacyText = "heritage-token Cafe\u0301\r\nlegacy line"
			const legacyVersion = "20000000-0000-4000-8000-000000000001"

			db, err := test.driver.Open(dbPath, docsqlite.OpenOptions{
				Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate,
			})
			require.NoError(t, err)
			result, err := db.Exec(`INSERT INTO extracted_text(
				blob_hash,extractor,extractor_version,status,error,attempts,text,extracted_at
			) VALUES(?, 'plain-text', 1, 'ok', NULL, 1, ?, ?)`,
				fixture.looseHash, legacyText, legacyMigrationTimestamp)
			require.NoError(t, err)
			rowID, err := result.LastInsertId()
			require.NoError(t, err)
			_, err = db.Exec(`INSERT INTO content_fts(rowid,blob_hash,extractor,text)
				VALUES(?,?,'plain-text',?)`, rowID, fixture.looseHash, legacyText)
			require.NoError(t, err)
			_, err = db.Exec(`INSERT INTO text_searchable_versions(version_id) VALUES(?)`, legacyVersion)
			require.NoError(t, err)
			var oldTextName string
			require.NoError(t, db.QueryRow(`
				SELECT n.name FROM content_fts
				JOIN content_versions v ON v.blob_hash=content_fts.blob_hash
				JOIN nodes n ON n.current_version_id=v.version_id
				JOIN text_searchable_versions sv ON sv.version_id=v.version_id
				WHERE content_fts MATCH '"heritage-token"*'
			`).Scan(&oldTextName))
			assert.Equal(t, "loose.txt", oldTextName)
			require.NoError(t, db.Close())

			s, err := Open(dbPath, test.driver)
			require.NoError(t, err)
			textHits, _, err := s.SearchPage(t.Context(), "heritage-token", 20)
			require.NoError(t, err)
			require.Len(t, textHits, 1)
			assert.Equal(t, "/loose.txt", textHits[0].Path)
			assert.Equal(t, SearchMatchContent, textHits[0].Match)
			nameHits, _, err := s.SearchPage(t.Context(), "loose", 20)
			require.NoError(t, err)
			require.Len(t, nameHits, 1)
			assert.Equal(t, "/loose.txt", nameHits[0].Path)
			assert.Equal(t, SearchMatchName, nameHits[0].Match)

			var builds, heads, legacyFTS int
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_builds`).Scan(&builds))
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_heads`).Scan(&heads))
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM content_fts`).Scan(&legacyFTS))
			assert.Equal(t, 1, builds)
			assert.Equal(t, 1, heads)
			assert.Zero(t, legacyFTS, "published current database serves only rendition authority")
			var unitID, text string
			require.NoError(t, s.db.QueryRow(`
				SELECT u.evidence_unit_id,l.text FROM rendition_units u
				JOIN rendition_lexical_segments l USING(build_id)
			`).Scan(&unitID, &text))
			assert.Equal(t, "legacy:0", unitID)
			assert.Equal(t, []byte(legacyText), []byte(text))
			require.NoError(t, s.Close())

			backup, err := test.driver.Open(dbPath+v090BackupSuffix, docsqlite.OpenOptions{
				Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Deferred,
			})
			require.NoError(t, err)
			var retainedText string
			require.NoError(t, backup.QueryRow(`
				SELECT text FROM extracted_text
				WHERE blob_hash=? AND extractor='plain-text'`, fixture.looseHash,
			).Scan(&retainedText))
			assert.Equal(t, []byte(legacyText), []byte(retainedText))
			require.NoError(t, backup.Close())
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

func TestOpenAcceptsCurrentSchemaColumnAddedToEmbeddedSchema(t *testing.T) {
	originalSchema := schemaSQL
	t.Cleanup(func() { schemaSQL = originalSchema })

	for _, table := range currentSchemaTables {
		for _, test := range v090UpgradeDrivers() {
			t.Run(table+"/"+test.name, func(t *testing.T) {
				schemaSQL = schemaSQLWithAddedColumn(t, originalSchema, table, "synthetic_schema_269")

				dbPath := filepath.Join(t.TempDir(), "docbank.db")
				s, err := Open(dbPath, test.driver)
				require.NoError(t, err)
				require.NoError(t, s.Close())

				reopened, err := Open(dbPath, test.driver)
				require.NoError(t, err)
				require.NoError(t, reopened.Close())
			})
		}
	}
}

func TestCanonicalCurrentSchemaDerivationDoesNotCacheErrors(t *testing.T) {
	originalSchema := schemaSQL
	t.Cleanup(func() { schemaSQL = originalSchema })
	schemaSQL = schemaSQLWithAddedColumn(t, originalSchema, "blobs", "synthetic_derivation_retry_269")
	driver := &flakySchemaDriver{Driver: DefaultSQLiteDriver()}

	_, err := canonicalCurrentSchemaColumns(driver)
	require.ErrorContains(t, err, "synthetic derivation failure")

	columns, err := canonicalCurrentSchemaColumns(driver)
	require.NoError(t, err)
	assert.Contains(t, columns["blobs"], "synthetic_derivation_retry_269")
}

type flakySchemaDriver struct {
	docsqlite.Driver

	failed bool
}

func (d *flakySchemaDriver) Open(path string, opts docsqlite.OpenOptions) (*sql.DB, error) {
	if !d.failed {
		d.failed = true
		return nil, errors.New("synthetic derivation failure")
	}
	return d.Driver.Open(path, opts)
}

func schemaSQLWithAddedColumn(t *testing.T, original, table, column string) string {
	t.Helper()
	lineEnding := "\n"
	if strings.Contains(original, "\r\n") {
		lineEnding = "\r\n"
	}
	result := strings.Replace(
		original,
		"CREATE TABLE IF NOT EXISTS "+table+" ("+lineEnding,
		"CREATE TABLE IF NOT EXISTS "+table+" ("+lineEnding+
			"    "+column+" TEXT,"+lineEnding,
		1,
	)
	require.NotEqual(t, original, result)
	return result
}

func TestOpenRejectsCurrentDatabaseWithForeignColumn(t *testing.T) {
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
			_, err = db.Exec(`ALTER TABLE blobs ADD COLUMN synthetic_unexpected_269 TEXT`)
			require.NoError(t, err)
			require.NoError(t, db.Close())

			_, err = Open(dbPath, test.driver)
			require.ErrorContains(t, err, "schema version 4 has an unexpected layout")
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
				kind, err := classifyDatabaseSchema(driver.driver, backup)
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
			kind, err := classifyDatabaseSchema(test.driver, backup)
			require.NoError(t, err)
			assert.Equal(t, 3, kind.version)
			assert.NotNil(t, kind.source)
			require.NoError(t, backup.Close())
		})
	}
}

// Coverage guard: the exact released v0.14 schema must carry its plain-text
// search result through the one-authority cutover in both SQLite modes.
func TestUpgradeReleasedV014MigratesPlainText(t *testing.T) {
	for _, test := range v090UpgradeDrivers() {
		t.Run(test.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "docbank.db")
			fixture := createV3Fixture(t, dbPath, test.driver)
			const (
				timestamp = "2026-08-16T12:00:00.000000000Z"
				versionID = "40000000-0000-4000-8000-000000000003"
				operation = "50000000-0000-4000-8000-000000000003"
				text      = "v014-search-token"
			)

			db, err := test.driver.Open(dbPath, docsqlite.OpenOptions{
				Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate,
			})
			require.NoError(t, err)
			tx, err := db.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			_, err = tx.Exec(`INSERT INTO nodes(
				id,parent_id,name,kind,current_version_id,revision,created_at,modified_at
			) VALUES(2,1,'released.txt','file',?,1,?,?)`, versionID, timestamp, timestamp)
			require.NoError(t, err)
			_, err = tx.Exec(`INSERT INTO content_versions(
				version_id,node_id,blob_hash,size,mime_type,recorded_at,node_revision,
				introduced_operation_id,transition_kind
			) VALUES(?,2,?,17,'text/plain',?,1,?,'content_create')`,
				versionID, fixture.blobHash, timestamp, operation)
			require.NoError(t, err)
			result, err := tx.Exec(`INSERT INTO extracted_text(
				blob_hash,extractor,extractor_version,status,error,attempts,text,extracted_at
			) VALUES(?,'plain-text',1,'ok',NULL,1,?,?)`, fixture.blobHash, text, timestamp)
			require.NoError(t, err)
			rowID, err := result.LastInsertId()
			require.NoError(t, err)
			_, err = tx.Exec(`INSERT INTO content_fts(rowid,blob_hash,extractor,text)
				VALUES(?,?,'plain-text',?)`, rowID, fixture.blobHash, text)
			require.NoError(t, err)
			_, err = tx.Exec(`INSERT INTO text_searchable_versions(version_id) VALUES(?)`, versionID)
			require.NoError(t, err)
			require.NoError(t, tx.Commit())
			require.NoError(t, db.Close())

			s, err := Open(dbPath, test.driver)
			require.NoError(t, err)
			hits, _, err := s.SearchPage(t.Context(), text, 20)
			require.NoError(t, err)
			require.Len(t, hits, 1)
			assert.Equal(t, "/released.txt", hits[0].Path)
			assert.Equal(t, SearchMatchContent, hits[0].Match)
			var heads, legacyFTS int
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_heads`).Scan(&heads))
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM content_fts`).Scan(&legacyFTS))
			assert.Equal(t, 1, heads)
			assert.Zero(t, legacyFTS)
			require.NoError(t, s.Close())

			backup, err := test.driver.Open(dbPath+v3BackupSuffix, docsqlite.OpenOptions{
				Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Deferred,
			})
			require.NoError(t, err)
			var retained string
			require.NoError(t, backup.QueryRow(`
				SELECT text FROM extracted_text
				WHERE blob_hash=? AND extractor='plain-text'`, fixture.blobHash,
			).Scan(&retained))
			assert.Equal(t, text, retained)
			require.NoError(t, backup.Close())
		})
	}
}

// Coverage guard: both released schema-v2 layouts must carry legacy text
// through the same one-authority cutover in both supported SQLite modes.
func TestUpgradeEveryReleasedSchemaV2LayoutMigratesLegacyPlainText(t *testing.T) {
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
				const legacyText = "schema-v2-heritage Cafe\u0301\r\nexact bytes"
				addV2LegacyPlainText(t, dbPath, driver.driver, fixture.rawHash, legacyText)

				s, err := Open(dbPath, driver.driver)
				require.NoError(t, err)
				var builds, heads, legacyFTS int
				require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_builds`).Scan(&builds))
				require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_heads`).Scan(&heads))
				require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM content_fts`).Scan(&legacyFTS))
				assert.Equal(t, 1, builds)
				assert.Equal(t, 1, heads)
				assert.Zero(t, legacyFTS)
				hits, _, err := s.SearchPage(t.Context(), "schema-v2-heritage", 20)
				require.NoError(t, err)
				require.Len(t, hits, 1)
				assert.Equal(t, "/raw.txt", hits[0].Path)
				require.NoError(t, s.Close())

				backup, err := driver.driver.Open(dbPath+v2BackupSuffix, docsqlite.OpenOptions{
					Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Deferred,
				})
				require.NoError(t, err)
				var retained string
				require.NoError(t, backup.QueryRow(`
					SELECT text FROM extracted_text
					WHERE blob_hash=? AND extractor='plain-text'`, fixture.rawHash,
				).Scan(&retained))
				assert.Equal(t, []byte(legacyText), []byte(retained))
				require.NoError(t, backup.Close())
			})
		}
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
	assertReleasedMetadataWithEmptyLexicalHead(t, fixture.metadata, metadata.Bytes())
	require.NoError(t, recovered.Close())
	_, err = os.Stat(stagePath)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(dbPath + sourceSchema.backupSuffix)
	require.NoError(t, err)
}

// Mutation caught: validating an interrupted current-schema stage without
// converging its legacy authority can publish a stage that was never cut over.
func TestInterruptedUpgradeStageMigratesLegacyBeforePublication(t *testing.T) {
	for _, test := range v090UpgradeDrivers() {
		t.Run(test.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "docbank.db")
			fixture := createV090Fixture(t, dbPath, test.driver)
			const legacyText = "interrupted-stage-heritage"
			addV090LegacyPlainText(t, dbPath, test.driver, fixture.looseHash, legacyText)
			sourceSchema := releasedStorageSchemas[0]
			stagePath := upgradeStagePath(dbPath, sourceSchema.version)
			jsonlPath := upgradeJSONLPath(dbPath, sourceSchema.version)

			source, err := openReleasedSource(dbPath, test.driver, sourceSchema)
			require.NoError(t, err)
			snapshot, err := source.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
			require.NoError(t, err)
			require.NoError(t, writeUpgradeJSONL(snapshot, jsonlPath, sourceSchema))
			target, err := openCurrentStore(stagePath, test.driver)
			require.NoError(t, err)
			require.NoError(t, importUpgradeJSONL(target, jsonlPath, sourceSchema))
			require.NoError(t, sourceSchema.restorePhysical(t.Context(), snapshot, target))
			require.NoError(t, target.Close())
			require.NoError(t, snapshot.Rollback())
			require.NoError(t, source.Close())

			require.NoError(t, validateUpgradeStage(stagePath, test.driver))
			stage, err := openCurrentStore(stagePath, test.driver)
			require.NoError(t, err)
			var heads, legacyFTS int
			require.NoError(t, stage.db.QueryRow(`SELECT COUNT(*) FROM rendition_heads`).Scan(&heads))
			require.NoError(t, stage.db.QueryRow(`SELECT COUNT(*) FROM content_fts`).Scan(&legacyFTS))
			assert.Equal(t, 1, heads)
			assert.Zero(t, legacyFTS)
			hits, _, err := stage.SearchPage(t.Context(), "interrupted-stage-heritage", 20)
			require.NoError(t, err)
			require.Len(t, hits, 1)
			require.NoError(t, stage.Close())
		})
	}
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
	kind, err := classifyDatabaseSchema(driver, db)
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
	require.NoError(t, exportReleasedMetadataSnapshot(t.Context(), snapshot, &metadata, 1))
	require.NoError(t, snapshot.Rollback())
	require.NoError(t, db.Close())
	return v090Fixture{
		looseHash: looseHash, packedHash: packedHash, packID: packID,
		deadPackID: deadPackID, metadata: metadata.Bytes(),
	}
}

func addV090LegacyPlainText(
	t *testing.T, path string, driver docsqlite.Driver, blobHash, text string,
) {
	t.Helper()
	db, err := driver.Open(path, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate,
	})
	require.NoError(t, err)
	result, err := db.Exec(`INSERT INTO extracted_text(
		blob_hash,extractor,extractor_version,status,error,attempts,text,extracted_at
	) VALUES(?,'plain-text',1,'ok',NULL,1,?,?)`, blobHash, text, legacyMigrationTimestamp)
	require.NoError(t, err)
	rowID, err := result.LastInsertId()
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO content_fts(rowid,blob_hash,extractor,text)
		VALUES(?,?,'plain-text',?)`, rowID, blobHash, text)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO text_searchable_versions(version_id)
		VALUES('20000000-0000-4000-8000-000000000001')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
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
	require.NoError(t, exportReleasedMetadataSnapshot(t.Context(), snapshot, &metadata, 2))
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
	require.NoError(t, exportReleasedMetadataSnapshot(t.Context(), snapshot, &metadata, 3))
	require.NoError(t, snapshot.Rollback())
	require.NoError(t, db.Close())
	return v3Fixture{
		blobHash: blobHash, primaryStoreID: primaryStoreID,
		secondaryStoreID: secondaryStoreID, metadata: metadata.Bytes(),
	}
}

func addV2LegacyPlainText(
	t *testing.T, path string, driver docsqlite.Driver, blobHash, text string,
) {
	t.Helper()
	db, err := driver.Open(path, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate,
	})
	require.NoError(t, err)
	const (
		timestamp = "2026-07-19T12:00:00.000000000Z"
		versionID = "20000000-0000-4000-8000-000000000001"
		operation = "30000000-0000-4000-8000-000000000001"
	)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.Exec(`PRAGMA defer_foreign_keys = ON`)
	require.NoError(t, err)
	_, err = tx.Exec(`INSERT INTO nodes(id,parent_id,name,kind,current_version_id,revision,
		created_at,modified_at) VALUES(2,1,'raw.txt','file',?,1,?,?)`,
		versionID, timestamp, timestamp)
	require.NoError(t, err)
	_, err = tx.Exec(`INSERT INTO content_versions(
		version_id,node_id,blob_hash,size,mime_type,recorded_at,node_revision,
		introduced_operation_id,transition_kind
	) VALUES(?,2,?,5,'text/plain',?,1,?,'content_create')`,
		versionID, blobHash, timestamp, operation)
	require.NoError(t, err)
	result, err := tx.Exec(`INSERT INTO extracted_text(
		blob_hash,extractor,extractor_version,status,error,attempts,text,extracted_at
	) VALUES(?,'plain-text',1,'ok',NULL,1,?,?)`, blobHash, text, timestamp)
	require.NoError(t, err)
	rowID, err := result.LastInsertId()
	require.NoError(t, err)
	_, err = tx.Exec(`INSERT INTO content_fts(rowid,blob_hash,extractor,text)
		VALUES(?,?,'plain-text',?)`, rowID, blobHash, text)
	require.NoError(t, err)
	_, err = tx.Exec(`INSERT INTO text_searchable_versions(version_id) VALUES(?)`, versionID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.NoError(t, db.Close())
}

func assertPhysicalContent(t *testing.T, s *Store, hash string, want PhysicalContent) {
	t.Helper()
	got, err := s.PhysicalContent(t.Context(), hash)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func assertReleasedMetadataWithEmptyLexicalHead(t *testing.T, released, current []byte) {
	t.Helper()
	var retained [][]byte
	var generations []metadataLexicalGeneration
	for line := range bytes.SplitSeq(bytes.TrimSpace(current), []byte{'\n'}) {
		var kind struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &kind))
		if kind.Type != metadataLexicalGenerationType {
			retained = append(retained, bytes.Clone(line))
			continue
		}
		var generation metadataLexicalGeneration
		require.NoError(t, json.Unmarshal(line, &generation))
		generations = append(generations, generation)
	}
	require.Len(t, generations, 1)
	require.NotEmpty(t, generations[0].BuiltAt)
	generations[0].BuiltAt = ""
	assert.Equal(t, metadataLexicalGeneration{
		Type:           metadataLexicalGenerationType,
		GenerationID:   "0d552c0f8d591c15930b04a8a529cb7fe71bc38fdbe44046206c8cf45480e187",
		SegmentCount:   0,
		ManifestDigest: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		BuildIDs:       []string{},
		BuildDigest:    "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		Headed:         true,
	}, generations[0])
	retainedMetadata := append(bytes.Join(retained, []byte{'\n'}), '\n')
	assert.Equal(t, released, retainedMetadata,
		"the released logical authority must survive byte-for-byte")
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
