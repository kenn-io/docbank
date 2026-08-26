package backupapp

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
	docsqlite "go.kenn.io/docbank/sqlite"
)

func TestRestoredContentPathsTreatsPreAuthoritySchemaBlobsAsLegacyOrdinary(t *testing.T) {
	driver := store.DefaultSQLiteDriver()
	db, err := driver.Open(t.TempDir()+"/legacy.db", docsqlite.OpenOptions{Access: docsqlite.Create})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, func() error {
		_, execErr := db.Exec(`CREATE TABLE blobs(hash TEXT PRIMARY KEY, size INTEGER NOT NULL)`)
		return execErr
	}())
	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	_, err = db.Exec(`INSERT INTO blobs(hash,size) VALUES(?,?)`, hash, 7)
	require.NoError(t, err)

	paths, err := restoredContentPaths(t.Context(), db, true)
	require.NoError(t, err)
	require.Equal(t, map[string][]string{hash: {"aa/" + hash}}, paths)
}
