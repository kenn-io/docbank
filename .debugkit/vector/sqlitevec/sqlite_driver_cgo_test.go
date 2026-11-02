//go:build !windows && cgo

package sqlitevec_test

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	vecext "github.com/asg017/sqlite-vec-go-bindings/cgo"
)

func openSQLiteTestDB(tb testing.TB, dsn string) (*sql.DB, error) {
	tb.Helper()
	vecext.Auto()
	return sql.Open("sqlite3", dsn)
}
