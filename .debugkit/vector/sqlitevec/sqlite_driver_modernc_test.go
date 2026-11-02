//go:build windows || !cgo

package sqlitevec_test

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	_ "modernc.org/sqlite/vec"
)

func openSQLiteTestDB(tb testing.TB, dsn string) (*sql.DB, error) {
	tb.Helper()
	return sql.Open("sqlite", dsn)
}
