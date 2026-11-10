package sqlite_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	docsqlite "go.kenn.io/docbank/sqlite"
	"go.kenn.io/docbank/sqlite/modernc"
)

func TestModerncNoBusyWait(t *testing.T) {
	assertNoBusyWait(t, modernc.Driver{})
}

// assertNoBusyWait checks that NoBusyWait turns off SQLite's busy handler, so
// a write that meets a held lock reports SQLITE_BUSY without sleeping.
func assertNoBusyWait(t *testing.T, driver docsqlite.Driver) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "busy.db")
	createContractDatabase(t, driver, path)
	holder := openAndPing(t, driver, path, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate,
	})
	waiter := openAndPing(t, driver, path, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Deferred, BusyTimeout: docsqlite.NoBusyWait,
	})
	require.Equal(t, int64(0), pragmaInt(t, waiter, "busy_timeout"), "NoBusyWait must disable the busy handler")

	tx, err := holder.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = waiter.ExecContext(t.Context(), `UPDATE contract SET value = 'blocked' WHERE id = 1`)
	require.Error(t, err)
	require.True(t, driver.IsBusy(err), "expected SQLITE_BUSY, got %v", err)
}
