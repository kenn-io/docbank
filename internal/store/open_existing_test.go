package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	docsqlite "go.kenn.io/docbank/sqlite"
)

func TestOpenExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "docbank.db")
	driver := DefaultSQLiteDriver()
	_, err := OpenExisting(t.Context(), path, driver)
	require.ErrorIs(t, err, ErrNeedsOwner)
	s, err := Open(t.Context(), path, driver)
	require.NoError(t, err)
	id, root := s.VaultID(), s.RootID()
	require.NoError(t, s.Close())
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	stale := upgradeStagePath(path, 28)
	require.NoError(t, os.WriteFile(stale, []byte("stale"), 0o600))
	s, err = OpenExisting(t.Context(), path, driver)
	require.NoError(t, err)
	require.Equal(t, id, s.VaultID())
	require.Equal(t, root, s.RootID())
	_, err = s.NodeByPath(t.Context(), "/")
	require.NoError(t, err)
	require.NoError(t, s.Close())
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after)
	data, err := os.ReadFile(stale)
	require.NoError(t, err)
	require.Equal(t, "stale", string(data))
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		require.NoError(t, os.WriteFile(path+suffix, nil, 0o600))
		_, err = OpenExisting(t.Context(), path, driver)
		require.ErrorIs(t, err, ErrNeedsOwner)
		require.NoError(t, os.Remove(path+suffix))
	}
}

func TestOpenExistingRefusesReleasedAndFresh(t *testing.T) {
	driver := DefaultSQLiteDriver()
	t.Run("released", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "docbank.db")
		db, _ := newReleasedFixtureStore(t, path, driver, schemaV0150SQL, 28)
		require.NoError(t, db.Close())
		_, err := OpenExisting(t.Context(), path, driver)
		require.ErrorIs(t, err, ErrNeedsOwner)
	})
	t.Run("fresh", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "docbank.db")
		db, err := driver.Open(path, docsqlite.OpenOptions{Access: docsqlite.Create, TransactionMode: docsqlite.Deferred})
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(), "CREATE TABLE unrelated (id INTEGER)")
		require.NoError(t, err)
		require.NoError(t, db.Close())
		_, err = OpenExisting(t.Context(), path, driver)
		require.ErrorIs(t, err, ErrNeedsOwner)
	})
}
