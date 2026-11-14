// Package pgtest owns disposable databases on the configured PostgreSQL test server.
package pgtest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

// NewDatabase creates a database on the explicitly configured test server.
// CI supplies a disposable cluster; local runs explicitly configure one.
// Every test owns a separate database, so it can exercise real role grants and
// failed DDL.
func NewDatabase(tb testing.TB) (*sql.DB, string) {
	tb.Helper()
	dsn := os.Getenv("DOCBANK_TEST_POSTGRES_DSN")
	if dsn == "" {
		tb.Skip("set DOCBANK_TEST_POSTGRES_DSN to run PostgreSQL tests")
	}
	c, err := pgx.ParseConfig(dsn)
	require.NoError(tb, err)
	admin := stdlib.OpenDB(*c)
	tb.Cleanup(func() { require.NoError(tb, admin.Close()) })
	name := fmt.Sprintf("db_%x", uuid.New())
	_, err = admin.ExecContext(tb.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	require.NoError(tb, err)
	tb.Cleanup(func() {
		// tb.Context is already canceled when cleanup runs.
		_, err := admin.ExecContext(context.WithoutCancel(tb.Context()), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		require.NoError(tb, err)
	})
	c.Database = name
	db := stdlib.OpenDB(*c)
	tb.Cleanup(func() { require.NoError(tb, db.Close()) })
	return db, c.User
}
