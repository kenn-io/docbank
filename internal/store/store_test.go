package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	docsqlite "go.kenn.io/docbank/sqlite"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

func TestStorageMutationDuringReadSnapshot(t *testing.T) {
	s := newTestStore(t)
	tx, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	var count int
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT count(*) FROM nodes`).Scan(&count))
	require.Equal(t, 1, count)

	_, err = s.Mkdir(t.Context(), s.RootID(), "inbox")
	require.NoError(t, err)
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT count(*) FROM nodes`).Scan(&count))
	require.Equal(t, 1, count, "the reader retains its original snapshot")
	require.NoError(t, tx.Commit())
	_, err = s.NodeByPath(t.Context(), "/inbox")
	require.NoError(t, err)
}

func TestStorageTransactionPreservesCancellationAfterAutomaticRollback(t *testing.T) {
	for _, phase := range []string{"callback", "commit"} {
		t.Run(phase, func(t *testing.T) {
			s := newTestStore(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
				cancel()
				// The only connection becomes available after automatic rollback.
				require.NoError(t, s.writeDB.PingContext(t.Context()))
				var value int
				readErr := tx.QueryRow("SELECT 1").Scan(&value)
				if phase == "callback" {
					return readErr
				}
				return nil
			})
			require.ErrorIs(t, err, context.Canceled)
			require.ErrorIs(t, err, sql.ErrTxDone)
		})
	}
}

func TestOpenRejectsObsoletePreReleaseSchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "obsolete.db")
	driver := DefaultSQLiteDriver()
	db, err := driver.Open(dbPath, docsqlite.OpenOptions{
		Access: docsqlite.Create, TransactionMode: docsqlite.Immediate,
	})
	require.NoError(t, err)
	_, err = db.Exec(`
		CREATE TABLE blobs (
			hash TEXT PRIMARY KEY, size INTEGER NOT NULL, created_at TEXT NOT NULL
		)`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	_, err = Open(dbPath, driver)
	require.ErrorContains(t, err, "unsupported schema")
}

func TestOpenBootstrapsRoot(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "docbank.db")
	s, err := Open(dbPath)
	require.NoError(t, err)
	rootID := s.RootID()
	assert.Positive(t, rootID)
	vaultID := s.VaultID()
	require.NoError(t, validateUUIDv4(vaultID))
	require.NoError(t, s.Close())

	// Reopen: same root, no duplicate.
	s2, err := Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, s2.Close()) }()
	assert.Equal(t, rootID, s2.RootID())
	assert.Equal(t, vaultID, s2.VaultID())

	var count int
	require.NoError(t, s2.db.QueryRow(
		`SELECT COUNT(*) FROM nodes WHERE parent_id IS NULL`).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestOpenConcurrentBootstrap(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "docbank.db")

	const n = 2
	var wg sync.WaitGroup
	stores := make([]*Store, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			stores[i], errs[i] = Open(dbPath)
		}(i)
	}
	wg.Wait()

	for i := range n {
		require.NoError(t, errs[i])
		assert.Equal(t, stores[0].VaultID(), stores[i].VaultID())
	}
	defer func() {
		for i := range n {
			require.NoError(t, stores[i].Close())
		}
	}()

	var count int
	require.NoError(t, stores[0].db.QueryRow(
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
