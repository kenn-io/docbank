package store

import (
	"bytes"
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	docsqlite "go.kenn.io/docbank/sqlite"
)

//go:embed testdata/schema-v0.15.0.sql
var schemaV0150SQL string

//go:embed testdata/schema-v0.15.1.sql
var schemaV0151SQL string

// releasedSchemaFixtures holds the exact schema.sql of every release from
// v0.15.0 on, keyed by storage schema version.
var releasedSchemaFixtures = map[int]string{28: schemaV0150SQL, 29: schemaV0151SQL}

// firstFixtureStorageSchemaVersion is the oldest schema in
// releasedSchemaFixtures. Older releases have their own upgrade tests.
const firstFixtureStorageSchemaVersion = 28

const releasedFixtureVaultID = "10000000-0000-4000-8000-000000000028"

// newReleasedFixtureStore creates a vault with an exact released schema and
// returns a Store bound to it. Seeding through current code is limited to
// tables whose layout matches the released one.
func newReleasedFixtureStore(t *testing.T, path string, driver docsqlite.Driver, schema string, version int) (*sql.DB, *Store) {
	t.Helper()
	db, err := driver.Open(path, docsqlite.OpenOptions{Access: docsqlite.Create, TransactionMode: docsqlite.Immediate})
	require.NoError(t, err)
	_, err = db.Exec(schema)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO vault_metadata(singleton,vault_uid,schema_version) VALUES(1,?,?)`, releasedFixtureVaultID, version)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO nodes(id,parent_id,name,kind,revision,created_at,modified_at)
		VALUES(1,NULL,'','dir',1,'2026-08-16T12:00:00.000000000Z','2026-08-16T12:00:00.000000000Z')`)
	require.NoError(t, err)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	primary, err := ensurePrimaryBlobStoreTx(tx)
	require.NoError(t, err)
	require.NoError(t, initializeDocumentPeopleState(t.Context(), tx))
	require.NoError(t, ensureProcessingIncarnationTx(tx, nil))
	require.NoError(t, tx.Commit())
	return db, &Store{db: db, writeDB: db, rootID: 1, vaultID: releasedFixtureVaultID, primaryStoreID: primary.ID, driver: driver}
}

func dumpReleasedOperationalTables(t *testing.T, q metadataQuerier) string {
	t.Helper()
	var out strings.Builder
	for _, table := range slices.Concat(releasedStorageOperationTables, releasedPendingDeletionTables) {
		dumpReleasedTable(t, q, table, &out)
	}
	return out.String()
}

func dumpReleasedTable(t *testing.T, q metadataQuerier, table string, out *strings.Builder) {
	t.Helper()
	columns, err := queryTableColumns(t.Context(), q, table)
	require.NoError(t, err)
	rows, err := q.QueryContext(t.Context(), `SELECT `+strings.Join(columns, ",")+` FROM `+table+` ORDER BY rowid`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	values := make([]any, len(columns))
	pointers := make([]any, len(values))
	for i := range values {
		pointers[i] = &values[i]
	}
	for rows.Next() {
		require.NoError(t, rows.Scan(pointers...))
		fmt.Fprintf(out, "%s %q\n", table, values)
	}
	require.NoError(t, rows.Err())
}

// seedV0150PendingWork records a photo, consent with a retrying rendition
// job, a queued photo import, its cleanup record, and every pending deletion.
func seedV0150PendingWork(t *testing.T, db *sql.DB, legacy *Store) (photoNodeID int64, jobID, operationID string) {
	t.Helper()
	ctx := t.Context()
	node, err := legacy.CreateFile(ctx, legacy.RootID(), "released-photo.raw", fakeHash("a1"), 4, "application/octet-stream")
	require.NoError(t, err)
	const assetID, fileID, receiptID = "40000000-0000-4000-8000-000000000001",
		"40000000-0000-4000-8000-000000000002", "40000000-0000-4000-8000-000000000003"
	const at = "2026-10-05T12:00:00.000000000Z"
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.Exec(`PRAGMA defer_foreign_keys=ON`)
	require.NoError(t, err)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO photo_assets(asset_id,kind,revision,display_file_id,created_at,updated_at) VALUES(?,'photo',1,?,?,?)`,
			[]any{assetID, fileID, at, at}},
		{`INSERT INTO photo_files(file_id,asset_id,node_id,role,created_at) VALUES(?,?,?,'image',?)`,
			[]any{fileID, assetID, node.ID, at}},
		{`INSERT INTO photo_change_receipts(receipt_id,operation,asset_id,before_revision,after_revision,before_json,after_json,created_at)
			VALUES(?,'create',?,0,1,'{}','{}',?)`, []any{receiptID, assetID, at}},
	} {
		_, err := tx.Exec(statement.query, statement.args...)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())

	now := time.Now().UTC().Add(time.Second)
	versions := seedRenditionCatalogVersions(t, legacy)
	request := renditionJobTestRequest(versions[0], catalogProcessingProfile(t, false))
	grantRenditionJobConsent(t, legacy, request)
	job, waiter, err := legacy.EnqueueRenditionJob(ctx, request)
	require.NoError(t, err)
	claim, err := legacy.ClaimRenditionJob(ctx, job.ID, "worker:released", now, time.Minute)
	require.NoError(t, err)
	_, err = legacy.BeginRenditionProvider(ctx, claim, waiter.ID, now.Add(time.Second), renditionJobTestSnapshot(request))
	require.NoError(t, err)
	require.NoError(t, legacy.CheckpointRenditionProvider(ctx, claim, "released-provider-handle", now.Add(2*time.Second)))
	require.NoError(t, legacy.MarkRenditionJobRetry(ctx, claim, RenditionFailureTransient, now.Add(3*time.Second), now.Add(4*time.Second)))

	operation, err := legacy.CreateLocalOperation(ctx, StorageOperationKindPhotoImport, `{}`)
	require.NoError(t, err)
	purged := fakeHash("b2")
	require.NoError(t, legacy.withStorageTx(ctx, func(tx *sql.Tx) error { return legacy.EnsureBlobTx(tx, purged, 9) }))
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO storage_operation_stores(operation_id,store_id,role) VALUES(?,?,'destination')`,
			[]any{operation.ID, legacy.primaryStoreID}},
		{`INSERT INTO storage_operation_cleanup(operation_id,store_id,loose_hash) VALUES(?,?,?)`,
			[]any{operation.ID, legacy.primaryStoreID, fakeHash("c3")}},
		{`INSERT INTO gc_loose_retirements(store_id,blob_hash,loose_encoding) VALUES(?,?,0)`,
			[]any{legacy.primaryStoreID, fakeHash("d4")}},
		{`INSERT INTO derivative_blob_purge_pending(blob_hash) VALUES(?)`, []any{purged}},
		{`INSERT INTO blob_packs(store_id,pack_id,entry_count,stored_bytes,created_at) VALUES(?,'purged-pack',0,0,?)`,
			[]any{legacy.primaryStoreID, at}},
		{`INSERT INTO derivative_pack_purge_pending(store_id,pack_id) VALUES(?,'purged-pack')`,
			[]any{legacy.primaryStoreID}},
	} {
		_, err := db.Exec(statement.query, statement.args...)
		require.NoError(t, err)
	}
	return node.ID, job.ID, operation.ID
}

func TestUpgradeReleasedV0150PreservesAuthorityAndPendingWork(t *testing.T) {
	t.Parallel()
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "docbank.db")
			db, legacy := newReleasedFixtureStore(t, path, driver.driver, schemaV0150SQL, 28)
			photoNodeID, jobID, operationID := seedV0150PendingWork(t, db, legacy)
			incarnation, err := legacy.CurrentProcessingIncarnation(t.Context())
			require.NoError(t, err)
			snapshot, err := db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
			require.NoError(t, err)
			var released bytes.Buffer
			require.NoError(t, exportReleasedMetadataSnapshot(t.Context(), snapshot, &released, 28))
			pending := dumpReleasedOperationalTables(t, snapshot)
			require.NoError(t, snapshot.Rollback())
			require.NoError(t, db.Close())
			require.Equal(t, len(releasedStorageOperationTables)+len(releasedPendingDeletionTables),
				strings.Count(pending, "\n"), "the fixture seeds one row in every copied table")

			s, err := Open(path, driver.driver)
			require.NoError(t, err)
			defer func() { require.NoError(t, s.Close()) }()

			var version int
			require.NoError(t, s.db.QueryRow(`SELECT schema_version FROM vault_metadata`).Scan(&version))
			assert.Equal(t, currentStorageSchemaVersion, version)
			var upgraded bytes.Buffer
			require.NoError(t, s.ExportMetadata(t.Context(), &upgraded))
			assert.Equal(t, released.String(), upgraded.String())
			assert.Equal(t, pending, dumpReleasedOperationalTables(t, s.db))

			current, err := s.CurrentProcessingIncarnation(t.Context())
			require.NoError(t, err)
			assert.Equal(t, incarnation, current, "processing consent survives the upgrade")
			resumedAt := time.Now().UTC().Add(10 * time.Second)
			claim, err := s.ClaimRenditionJob(t.Context(), jobID, "worker:upgraded", resumedAt, time.Minute)
			require.NoError(t, err)
			_, err = s.RenditionJobWorkByClaim(t.Context(), claim, resumedAt)
			require.NoError(t, err, "a pending rendition job keeps its authorization")

			resumable, err := s.ResumableStorageOperations(t.Context())
			require.NoError(t, err)
			require.Len(t, resumable, 1)
			assert.Equal(t, operationID, resumable[0].ID)

			asset, err := s.PhotoAssetForNode(t.Context(), photoNodeID)
			require.NoError(t, err)
			assert.Equal(t, "40000000-0000-4000-8000-000000000001", asset.ID)
			require.FileExists(t, path+v28BackupSuffix)
		})
	}
}

// Coverage guard: each released schema still upgrades to the current one.
// When the current schema moves past a fixture's version, this test fails
// until that version has a cutover adapter.
func TestEveryReleasedSchemaFixtureUpgrades(t *testing.T) {
	t.Parallel()
	for version, schema := range releasedSchemaFixtures {
		for _, driver := range v090UpgradeDrivers() {
			t.Run(fmt.Sprintf("v%d/%s", version, driver.name), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "docbank.db")
				db, legacy := newReleasedFixtureStore(t, path, driver.driver, schema, version)
				_, err := legacy.CreateFile(t.Context(), legacy.RootID(), "released.txt", fakeHash("e5"), 8, "text/plain")
				require.NoError(t, err)
				require.NoError(t, db.Close())
				s, err := Open(path, driver.driver)
				require.NoError(t, err)
				defer func() { require.NoError(t, s.Close()) }()
				var current int
				require.NoError(t, s.db.QueryRow(`SELECT schema_version FROM vault_metadata`).Scan(&current))
				assert.Equal(t, currentStorageSchemaVersion, current)
			})
		}
	}
}

// Coverage guard: a released schema is frozen. Changing schema.sql after a
// release requires a new storage schema version and a cutover adapter.
func TestReleasedSchemaFixturesMatchVersions(t *testing.T) {
	t.Parallel()
	for _, version := range releasedStorageSchemaVersions {
		if version < firstFixtureStorageSchemaVersion {
			continue
		}
		fixture, ok := releasedSchemaFixtures[version]
		require.Truef(t, ok, "released storage schema %d needs an exact fixture in releasedSchemaFixtures", version)
		if version == currentStorageSchemaVersion {
			assert.Equal(t, fixture, schemaSQL,
				"schema.sql differs from released schema %d; bump currentStorageSchemaVersion and add a cutover adapter", version)
		}
	}
	for version := range releasedSchemaFixtures {
		assert.Contains(t, releasedStorageSchemaVersions, version)
	}
}

func TestReleasedStorageSchemasRequireAdapters(t *testing.T) { //nolint:paralleltest // swaps the package-level releasedStorageSchemaVersions
	require.NoError(t, validateReleasedStorageSchemas())
	original := releasedStorageSchemaVersions
	t.Cleanup(func() { releasedStorageSchemaVersions = original })

	releasedStorageSchemaVersions = append(append([]int(nil), original...), 27)
	require.ErrorContains(t, validateReleasedStorageSchemas(), "released storage schema version 27 adapter is missing")

	releasedStorageSchemaVersions = []int{1, 2, 3, 29}
	require.ErrorContains(t, validateReleasedStorageSchemas(), "storage schema version 28 has an adapter but no release")
}

func TestOpenRejectsChangedReleasedV0150Layout(t *testing.T) {
	t.Parallel()
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "docbank.db")
			db, _ := newReleasedFixtureStore(t, path, driver.driver, schemaV0150SQL, 28)
			_, err := db.Exec(`ALTER TABLE storage_operations ADD COLUMN unexpected TEXT`)
			require.NoError(t, err)
			require.NoError(t, db.Close())
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			s, err := Open(path, driver.driver)
			if s != nil {
				require.NoError(t, s.Close())
			}
			require.ErrorContains(t, err, "not a released v0.15.0 database: unexpected storage_operations columns")
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, before, after, "a refused upgrade leaves the vault unchanged")
		})
	}
}

func TestImportMetadataProcessingIncarnationAcceptsOnlyIdenticalExistingRow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	current := metadataProcessingIncarnation{Type: metadataProcessingIncarnationType}
	require.NoError(t, s.db.QueryRow(`SELECT incarnation_id,created_at FROM processing_incarnations`).
		Scan(&current.ID, &current.CreatedAt))
	tx, err := s.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback()) }()

	require.NoError(t, importMetadataProcessingIncarnation(t.Context(), tx, current))
	changed := current
	changed.CreatedAt = "2026-01-01T00:00:00.000000000Z"
	require.ErrorContains(t, importMetadataProcessingIncarnation(t.Context(), tx, changed),
		"processing incarnation "+current.ID+" already exists with created_at")
	older := metadataProcessingIncarnation{Type: metadataProcessingIncarnationType,
		ID: "60000000-0000-4000-8000-000000000001", CreatedAt: "2026-01-01T00:00:00.000000000Z"}
	require.NoError(t, importMetadataProcessingIncarnation(t.Context(), tx, older))

	var count int
	require.NoError(t, tx.QueryRow(`SELECT COUNT(*) FROM processing_incarnations`).Scan(&count))
	assert.Equal(t, 2, count)
}
