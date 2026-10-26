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
func NewDatabase(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dsn := os.Getenv("DOCBANK_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set DOCBANK_TEST_POSTGRES_DSN to run PostgreSQL tests")
	}
	c, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	admin := stdlib.OpenDB(*c)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })
	name := fmt.Sprintf("db_%x", uuid.New())
	_, err = admin.ExecContext(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.ExecContext(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		require.NoError(t, err)
	})
	c.Database = name
	db := stdlib.OpenDB(*c)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db, c.User
}
