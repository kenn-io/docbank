package store

import (
	"bytes"
	"database/sql"
	_ "embed"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	docsqlite "go.kenn.io/docbank/sqlite"
)

//go:embed testdata/schema-v0.15.1.sql
var schemaV0151SQL string

func TestPhotoHiddenUpgradeReleasedV0151(t *testing.T) {
	t.Parallel()
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "docbank.db")
			db, err := driver.driver.Open(path, docsqlite.OpenOptions{Access: docsqlite.Create, TransactionMode: docsqlite.Immediate})
			require.NoError(t, err)
			_, err = db.Exec(schemaV0151SQL)
			require.NoError(t, err)
			_, err = db.Exec(`INSERT INTO vault_metadata(singleton,vault_uid,schema_version) VALUES(1,'10000000-0000-4000-8000-000000000003',29)`)
			require.NoError(t, err)
			_, err = db.Exec(`INSERT INTO nodes(id,parent_id,name,kind,revision,created_at,modified_at) VALUES(1,NULL,'','dir',1,'2026-08-16T12:00:00.000000000Z','2026-08-16T12:00:00.000000000Z')`)
			require.NoError(t, err)
			tx, err := db.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			primary, err := ensurePrimaryBlobStoreTx(tx)
			require.NoError(t, err)
			require.NoError(t, initializeDocumentPeopleState(t.Context(), tx))
			require.NoError(t, ensureProcessingIncarnationTx(tx))
			require.NoError(t, tx.Commit())
			legacy := &Store{db: db, writeDB: db, rootID: 1, vaultID: "10000000-0000-4000-8000-000000000003", primaryStoreID: primary.ID, driver: driver.driver}
			node, err := legacy.CreateFile(t.Context(), 1, "released-photo.jpg", fakeHash("a1"), 4, "image/jpeg")
			require.NoError(t, err)
			snapshot, err := db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
			require.NoError(t, err)
			var before bytes.Buffer
			require.NoError(t, exportReleasedMetadataSnapshot(t.Context(), snapshot, &before, 29))
			require.NoError(t, snapshot.Rollback())
			require.NoError(t, db.Close())
			s, err := Open(path, driver.driver)
			require.NoError(t, err)
			defer func() { require.NoError(t, s.Close()) }()
			var version int
			require.NoError(t, s.db.QueryRow(`SELECT schema_version FROM vault_metadata`).Scan(&version))
			assert.Equal(t, 30, version)
			var after bytes.Buffer
			require.NoError(t, s.ExportMetadata(t.Context(), &after))
			assert.Equal(t, before.String(), after.String())
			asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
			require.NoError(t, err)
			assert.Nil(t, asset.HiddenAt)
			assert.Equal(t, int64(1), asset.Revision)
			assert.Equal(t, node.ID, asset.Files[0].NodeID)
			require.FileExists(t, path+".schema-v29.bak")
			require.NoError(t, s.SetupPhotoHidden(t.Context(), "synthetic"))
		})
	}
}

func TestPhotoHiddenUpgradeRejectsChangedReleasedColumns(t *testing.T) {
	t.Parallel()
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			t.Parallel()
			db, err := driver.driver.Open(filepath.Join(t.TempDir(), "released.db"), docsqlite.OpenOptions{Access: docsqlite.Create, TransactionMode: docsqlite.Immediate})
			require.NoError(t, err)
			defer func() { require.NoError(t, db.Close()) }()
			_, err = db.Exec(schemaV0151SQL)
			require.NoError(t, err)
			require.NoError(t, validateV29Schema(db, nil, nil))
			_, err = db.Exec(`ALTER TABLE photo_files RENAME COLUMN role TO unsupported_role`)
			require.NoError(t, err)
			require.ErrorContains(t, validateV29Schema(db, nil, nil), "photo_files")
		})
	}
}
