package processing

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
	docsqlite "go.kenn.io/docbank/sqlite"
	"os"
	"path/filepath"
	"testing"
)

// Seed the exact oldest supported released schema with real synthetic EML
// bytes, then enter the ordinary current-schema upgrade before rendering.
func releasedEmailPDFFixture(t *testing.T) (emailPipelineFixture, store.EmailTarget) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	dbPath := filepath.Join(root, "docbank.db")
	driver := store.DefaultSQLiteDriver()
	db, err := driver.Open(dbPath, docsqlite.OpenOptions{Access: docsqlite.Create, TransactionMode: docsqlite.Immediate})
	require.NoError(t, err)
	schema, err := os.ReadFile(filepath.Join("..", "store", "testdata", "schema-v0.9.0.sql"))
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), string(schema))
	require.NoError(t, err)
	const stamp = "2026-07-19T12:00:00.000000000Z"
	const version = "20000000-0000-4000-8000-000000000001"
	hash := renditionBytesSHA256([]byte(emailPipelineSource))
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO vault_metadata(singleton,vault_id) VALUES(1,?)`, []any{"10000000-0000-4000-8000-000000000001"}},
		{`INSERT INTO blobs(hash,size,created_at) VALUES(?,?,?)`, []any{hash, len(emailPipelineSource), stamp}},
		{`INSERT INTO nodes(id,parent_id,name,kind,current_version_id,revision,created_at,modified_at) VALUES(1,NULL,'','dir',NULL,1,?,?)`, []any{stamp, stamp}},
		{`INSERT INTO nodes(id,parent_id,name,kind,current_version_id,revision,created_at,modified_at) VALUES(2,1,'synthetic.eml','file',?,1,?,?)`, []any{version, stamp, stamp}},
		{`INSERT INTO content_versions(version_id,node_id,blob_hash,size,mime_type,recorded_at,node_revision,introduced_operation_id,transition_kind) VALUES(?,2,?,?,'message/rfc822',?,1,?,'content_create')`, []any{version, hash, len(emailPipelineSource), stamp, "30000000-0000-4000-8000-000000000001"}},
	} {
		_, err := tx.ExecContext(t.Context(), statement.query, statement.args...)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	require.NoError(t, db.Close())
	loose := filepath.Join(root, "blobs", hash[:2], hash)
	require.NoError(t, os.MkdirAll(filepath.Dir(loose), 0700))
	require.NoError(t, os.WriteFile(loose, []byte(emailPipelineSource), 0600))
	catalog, err := store.Open(dbPath, driver)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	spool := filepath.Join(root, "spools")
	require.NoError(t, os.Mkdir(spool, 0700))
	v, err := catalog.ContentVersionByID(t.Context(), version)
	require.NoError(t, err)
	require.Equal(t, hash, v.BlobHash)
	return emailPipelineFixture{catalog, blobs, spool}, store.EmailTarget{Version: v}
}
