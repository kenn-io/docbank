package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	docsqlite "go.kenn.io/docbank/sqlite"
)

func TestOpenExisting(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "docbank.db")
	driver := DefaultSQLiteDriver()
	scratchDir := t.TempDir()
	_, err := OpenExisting(t.Context(), path, driver, scratchDir)
	require.ErrorIs(t, err, ErrNeedsOwner)
	s, err := Open(t.Context(), path, driver)
	require.NoError(t, err)
	id, root := s.VaultID(), s.RootID()
	require.NoError(t, s.Close())
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	stale := upgradeStagePath(path, 28)
	require.NoError(t, os.WriteFile(stale, []byte("stale"), 0o600))
	s, err = OpenExisting(t.Context(), path, driver, scratchDir)
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
		_, err = OpenExisting(t.Context(), path, driver, scratchDir)
		require.ErrorIs(t, err, ErrNeedsOwner)
		require.NoError(t, os.Remove(path+suffix))
	}
}

func TestOpenExistingRefusesReleasedAndFresh(t *testing.T) {
	t.Parallel()
	driver := DefaultSQLiteDriver()
	scratchDir := t.TempDir()
	t.Run("released", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "docbank.db")
		db, _ := newReleasedFixtureStore(t, path, driver, schemaV0150SQL, 28)
		require.NoError(t, db.Close())
		_, err := OpenExisting(t.Context(), path, driver, scratchDir)
		require.ErrorIs(t, err, ErrNeedsOwner)
	})
	t.Run("fresh", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "docbank.db")
		db, err := driver.Open(path, docsqlite.OpenOptions{Access: docsqlite.Create, TransactionMode: docsqlite.Deferred})
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(), "CREATE TABLE unrelated (id INTEGER)")
		require.NoError(t, err)
		require.NoError(t, db.Close())
		_, err = OpenExisting(t.Context(), path, driver, scratchDir)
		require.ErrorIs(t, err, ErrNeedsOwner)
	})
}

func TestOpenExistingScratchBoundary(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	path := filepath.Join(root, "docbank.db")
	for _, scratch := range []string{"", root, filepath.Join(root, "missing"), alias} {
		_, err := OpenExisting(t.Context(), path, DefaultSQLiteDriver(), scratch)
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrNeedsOwner)
	}
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestOpenExistingScratchUsesProvidedDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	scratch := t.TempDir()
	path := filepath.Join(root, "docbank.db")
	driver := &recordingScratchDriver{Driver: DefaultSQLiteDriver()}
	owner, err := Open(t.Context(), path, driver)
	require.NoError(t, err)
	require.NoError(t, owner.Close())
	driver.paths = nil
	reader, err := OpenExisting(t.Context(), path, driver, scratch)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.Len(t, driver.paths, 1)
	canonical, err := filepath.EvalSymlinks(scratch)
	require.NoError(t, err)
	require.Equal(t, canonical, filepath.Dir(driver.paths[0]))
	entries, err := os.ReadDir(scratch)
	require.NoError(t, err)
	require.Empty(t, entries)
}

type recordingScratchDriver struct {
	docsqlite.Driver
	paths []string
}

func (d *recordingScratchDriver) Open(path string, opts docsqlite.OpenOptions) (*sql.DB, error) {
	if opts.Access == docsqlite.Create {
		d.paths = append(d.paths, path)
	}
	return d.Driver.Open(path, opts)
}

func TestOpenExistingRejectsNonRegularDatabase(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "docbank.db")
	require.NoError(t, os.Mkdir(path, 0o700))
	_, err := OpenExisting(t.Context(), path, DefaultSQLiteDriver(), t.TempDir())
	require.ErrorContains(t, err, "not a regular file")
	require.NotErrorIs(t, err, ErrNeedsOwner)
}
