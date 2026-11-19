//go:build cgo

package mattn

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDisableMemoryStatusWithOpenConnection(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	_, err = conn.ExecContext(t.Context(), "CREATE TABLE kept (n); INSERT INTO kept VALUES (42)")
	require.NoError(t, err)

	disableMemoryStatus()

	var value int
	require.NoError(t, conn.QueryRowContext(t.Context(), "SELECT n FROM kept").Scan(&value))
	require.Equal(t, 42, value)
}
