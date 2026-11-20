//go:build cgo

package mattn

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	docsqlite "go.kenn.io/docbank/sqlite"
)

// Memory statistics and heap limits are process-wide and the limit cannot be
// lowered back, so each scenario runs in its own copy of the test binary.
const memstatusWorker = "DOCBANK_TEST_MATTN_MEMSTATUS_WORKER"

func inMemstatusWorker(t *testing.T) bool {
	t.Helper()
	if os.Getenv(memstatusWorker) == t.Name() {
		return true
	}
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v") //nolint:gosec // Runs this test binary with the current test's name.
	command.Env = append(os.Environ(), memstatusWorker+"="+t.Name())
	output, err := command.CombinedOutput()
	t.Log(string(output))
	require.NoError(t, err)
	return false
}

func openDocbank(t *testing.T) {
	t.Helper()
	db, err := Driver{}.Open(filepath.Join(t.TempDir(), "docbank.db"), docsqlite.OpenOptions{
		Access: docsqlite.Create, TransactionMode: docsqlite.Immediate,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, db.PingContext(t.Context()))
}

// openApplicationConn opens a connection the way an embedding application
// would, through mattn's own driver registration.
func openApplicationConn(t *testing.T) *sql.Conn {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	return conn
}

// allocateOverHardHeapLimit sets a 1 MiB hard heap limit, which SQLite
// enforces only while memory statistics are on, and asks for a 4 MiB blob.
func allocateOverHardHeapLimit(t *testing.T, conn *sql.Conn) error {
	t.Helper()
	ctx := t.Context()
	_, err := conn.ExecContext(ctx, "PRAGMA hard_heap_limit = 1048576")
	require.NoError(t, err)
	var length int
	err = conn.QueryRowContext(ctx, "SELECT length(randomblob(4194304))").Scan(&length)
	if err == nil {
		require.Equal(t, 4194304, length)
	}
	return err
}

func TestDocbankOpenKeepsApplicationHeapLimit(t *testing.T) {
	t.Parallel()
	if !inMemstatusWorker(t) {
		return
	}
	conn := openApplicationConn(t)
	openDocbank(t)

	require.ErrorContains(t, allocateOverHardHeapLimit(t, conn), "out of memory")
}

func TestFirstDocbankOpenDisablesMemoryStatus(t *testing.T) {
	t.Parallel()
	if !inMemstatusWorker(t) {
		return
	}
	openDocbank(t)
	conn := openApplicationConn(t)

	require.NoError(t, allocateOverHardHeapLimit(t, conn))
}
