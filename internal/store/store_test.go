package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

func TestOpenBootstrapsRoot(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "docbank.db")
	s, err := Open(dbPath)
	require.NoError(t, err)
	rootID := s.RootID()
	assert.Positive(t, rootID)
	require.NoError(t, s.Close())

	// Reopen: same root, no duplicate.
	s2, err := Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, s2.Close()) }()
	assert.Equal(t, rootID, s2.RootID())

	var count int
	require.NoError(t, s2.db.QueryRow(
		`SELECT COUNT(*) FROM nodes WHERE parent_id IS NULL`).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestSchemaForbidsSecondRoot(t *testing.T) {
	s := newTestStore(t)
	_, err := s.db.Exec(
		`INSERT INTO nodes (parent_id, name, kind, created_at, modified_at)
		 VALUES (NULL, 'root2', 'dir', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UNIQUE")
}
