package store

import (
	_ "embed"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	docsqlite "go.kenn.io/docbank/sqlite"
)

//go:embed testdata/schema-v0.15.0.sql
var schemaV0150SQL string

func TestPhotoUpgradeSchema28(t *testing.T) {
	t.Parallel()
	for _, test := range v090UpgradeDrivers() {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "docbank.db")
			db, err := test.driver.Open(path, docsqlite.OpenOptions{Access: docsqlite.Create, TransactionMode: docsqlite.Immediate})
			require.NoError(t, err)
			_, err = db.Exec(schemaV0150SQL)
			require.NoError(t, err)
			const stamp = "2026-08-16T12:00:00.000000000Z"
			const vault = "10000000-0000-4000-8000-000000000003"
			const storeID = "20000000-0000-4000-8000-000000000003"
			const version = "30000000-0000-4000-8000-000000000003"
			const operation = "40000000-0000-4000-8000-000000000003"
			const asset = "50000000-0000-4000-8000-000000000003"
			const file = "60000000-0000-4000-8000-000000000003"
			tx, err := db.Begin()
			require.NoError(t, err)
			_, err = tx.Exec(`PRAGMA defer_foreign_keys=ON`)
			require.NoError(t, err)
			_, err = tx.Exec(`INSERT INTO vault_metadata(singleton,vault_uid,schema_version) VALUES(1,?,28)`, vault)
			require.NoError(t, err)
			_, err = tx.Exec(`INSERT INTO nodes(id,parent_id,name,kind,current_version_id,revision,created_at,modified_at) VALUES(1,NULL,'','dir',NULL,1,?,?),(2,1,'photo.jpg','file',?,1,?,?)`, stamp, stamp, version, stamp, stamp)
			require.NoError(t, err)
			_, err = tx.Exec(`INSERT INTO blobs(hash,size,created_at) VALUES(?,4,?)`, fakeHash("a1"), stamp)
			require.NoError(t, err)
			_, err = tx.Exec(`INSERT INTO content_versions(version_id,node_id,blob_hash,size,mime_type,recorded_at,node_revision,introduced_operation_id,transition_kind) VALUES(?,2,?,4,'image/jpeg',?,1,?,'content_create')`, version, fakeHash("a1"), stamp, operation)
			require.NoError(t, err)
			_, err = tx.Exec(`INSERT INTO blob_stores(store_id,name,kind,role,lifecycle,binding,ownership_epoch,created_at) VALUES(?,'primary','filesystem','primary','managed','{}',?,?)`, storeID, operation, stamp)
			require.NoError(t, err)
			_, err = tx.Exec(`INSERT INTO blob_locations(blob_hash,store_id,generation,kind,encoding,stored_size,pack_eligible) VALUES(?,?,'1','loose','raw',4,1)`, fakeHash("a1"), storeID)
			require.NoError(t, err)
			_, err = tx.Exec(`INSERT INTO photo_assets(asset_id,kind,revision,display_file_id,created_at,updated_at) VALUES(?,'photo',1,?,?,?)`, asset, file, stamp, stamp)
			require.NoError(t, err)
			_, err = tx.Exec(`INSERT INTO photo_files(file_id,asset_id,node_id,role,created_at) VALUES(?,?,2,'image',?)`, file, asset, stamp)
			require.NoError(t, err)
			require.NoError(t, ensureProcessingIncarnationTx(tx))
			require.NoError(t, tx.Commit())
			require.NoError(t, db.Close())
			s, err := Open(path, test.driver)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			got, err := s.PhotoAssetByID(t.Context(), asset)
			require.NoError(t, err)
			require.Len(t, got.Files, 1)
			assert.Equal(t, file, got.Files[0].ID)
			assert.Equal(t, int64(1), got.Files[0].Revision)
			assert.Equal(t, PhotoAuthored{}, got.Files[0].Authored())
			require.NoError(t, s.ValidateMetadata(t.Context()))
			auditFolder, err := s.Mkdir(t.Context(), s.RootID(), "Audited")
			require.NoError(t, err)
			photoFolder, err := s.Mkdir(t.Context(), s.RootID(), "Photos")
			require.NoError(t, err)
			_, _, err = s.Move(t.Context(), 2, photoFolder.ID, "photo.jpg", 1)
			require.NoError(t, err)
			seedInitialAuditAuthority(t, s, auditFolder.ID)
			plan, err := s.PreviewInitialAudit(t.Context(), photoFolder.ID, "cli", nil)
			require.NoError(t, err)
			_, err = s.EnableInitialAudit(t.Context(), plan)
			require.NoError(t, err)
			require.NoError(t, s.ValidateMetadata(t.Context()))
		})
	}
}
