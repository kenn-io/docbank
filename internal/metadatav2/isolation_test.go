package metadatav2_test

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func TestNormalStoreOpenDoesNotActivateV2(t *testing.T) {
	path := t.TempDir() + "/docbank.db"
	v1, err := store.Open(path)
	require.NoError(t, err)
	require.NoError(t, v1.Close())

	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	var count int
	err = db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type='table' AND name='vault_metadata'
	`).Scan(&count)
	require.NoError(t, err)
	assert.Zero(t, count, "metadata v2 must remain dormant until the fenced cutover")
}
