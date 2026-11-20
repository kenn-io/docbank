package sqlite_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	docsqlite "go.kenn.io/docbank/sqlite"
	"go.kenn.io/docbank/sqlite/modernc"
)

func TestModerncStatementReuse(t *testing.T) {
	assertStatementReuse(t, modernc.Driver{})
}

// assertStatementReuse runs repeated statements on one connection, where a
// driver's per-connection statement cache would hand back a prepared
// statement, and checks every result.
func assertStatementReuse(t *testing.T, driver docsqlite.Driver) {
	t.Helper()
	db := openAndPing(t, driver, filepath.Join(t.TempDir(), "reuse.db"),
		docsqlite.OpenOptions{Access: docsqlite.Create, TransactionMode: docsqlite.Deferred})
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	_, err = conn.ExecContext(t.Context(), "CREATE TABLE ordered(v INTEGER); INSERT INTO ordered VALUES (1),(2),(3); CREATE TABLE once(k INTEGER UNIQUE)")
	require.NoError(t, err)

	t.Run("same query while its rows are open", func(t *testing.T) {
		const query = "SELECT v FROM ordered ORDER BY v"
		// The first run leaves an idle prepared statement for the outer query.
		require.Equal(t, []int{1, 2, 3}, queryInts(t, conn, query))
		outer, err := conn.QueryContext(t.Context(), query)
		require.NoError(t, err)
		defer func() { require.NoError(t, outer.Close()) }()
		require.True(t, outer.Next())
		var first int
		require.NoError(t, outer.Scan(&first))
		require.Equal(t, 1, first)
		require.Equal(t, []int{1, 2, 3}, queryInts(t, conn, query))
		var rest []int
		for outer.Next() {
			var v int
			require.NoError(t, outer.Scan(&v))
			rest = append(rest, v)
		}
		require.NoError(t, outer.Err())
		require.Equal(t, []int{2, 3}, rest)
	})

	t.Run("failed statement runs again", func(t *testing.T) {
		const insert = "INSERT INTO once(k) VALUES (?)"
		_, err := conn.ExecContext(t.Context(), insert, 1)
		require.NoError(t, err)
		_, err = conn.ExecContext(t.Context(), insert, 1)
		require.True(t, driver.IsUniqueViolation(err), "duplicate key error: %v", err)
		_, err = conn.ExecContext(t.Context(), insert, 2)
		require.NoError(t, err)
		require.Equal(t, []int{1, 2}, queryInts(t, conn, "SELECT k FROM once ORDER BY k"))
	})

	t.Run("more distinct statements than a cache holds", func(t *testing.T) {
		// 300 exceeds both adapters' 128-statement caches, so later queries
		// evict earlier ones and the second pass mixes misses and hits.
		for range 2 {
			for i := range 300 {
				require.Equal(t, []int{i}, queryInts(t, conn, fmt.Sprintf("SELECT %d", i)))
			}
		}
	})
}

func queryInts(t *testing.T, conn *sql.Conn, query string) []int {
	t.Helper()
	rows, err := conn.QueryContext(t.Context(), query)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var values []int
	for rows.Next() {
		var v int
		require.NoError(t, rows.Scan(&v))
		values = append(values, v)
	}
	require.NoError(t, rows.Err())
	return values
}
