package docbank

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/pgtest"
)

func TestGrantRuntimePostgres(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	admin, _ := pgtest.NewDatabase(t)
	var database string
	require.NoError(t, admin.QueryRowContext(ctx, "SELECT current_database()").Scan(&database))
	quotedDatabase := pgx.Identifier{database}.Sanitize()
	ownerRole := "owner_" + uuid.New().String()
	runtimeRole := "runtime_" + uuid.New().String()
	noLoginRole := "no_login_" + uuid.New().String()
	quotedOwner := pgx.Identifier{ownerRole}.Sanitize()
	quotedRuntime := pgx.Identifier{runtimeRole}.Sanitize()
	quotedNoLogin := pgx.Identifier{noLoginRole}.Sanitize()

	exec := func(db *sql.DB, query string) {
		t.Helper()
		_, err := db.ExecContext(ctx, query)
		require.NoError(t, err)
	}
	exec(admin, "CREATE ROLE "+quotedOwner+" LOGIN PASSWORD 'fixture'")
	exec(admin, "CREATE ROLE "+quotedRuntime+" LOGIN PASSWORD 'fixture'")
	exec(admin, "CREATE ROLE "+quotedNoLogin+" NOLOGIN")
	t.Cleanup(func() {
		cleanupCtx := context.WithoutCancel(ctx)
		roles := quotedOwner + "," + quotedRuntime + "," + quotedNoLogin
		_, err := admin.ExecContext(cleanupCtx, "DROP OWNED BY "+roles+"; DROP ROLE "+roles)
		require.NoError(t, err)
	})
	exec(admin, "GRANT CREATE ON DATABASE "+quotedDatabase+" TO "+quotedOwner)
	exec(admin, "REVOKE TEMPORARY ON DATABASE "+quotedDatabase+" FROM PUBLIC")

	connect := func(role string) *sql.DB {
		t.Helper()
		cfg := pgtest.ServerConfig(t)
		cfg.Database = database
		cfg.User = role
		cfg.Password = "fixture"
		db := stdlib.OpenDB(*cfg)
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		return db
	}
	owner := connect(ownerRole)
	runtime := connect(runtimeRole)

	catalog := PostgresCatalog{Schema: "documents", SchemaOwner: ownerRole}
	root := filepath.Join(t.TempDir(), "vault")
	release := pgtest.ReserveSchemaInstall(t)
	_, err := InitializePostgres(ctx, owner, catalog, root)
	release()
	require.NoError(t, err)
	require.ErrorContains(t, GrantPostgresRuntime(ctx, owner, catalog, runtimeRole), "TEMPORARY")
	exec(admin, "GRANT TEMPORARY ON DATABASE "+quotedDatabase+" TO "+quotedRuntime)
	require.NoError(t, GrantPostgresRuntime(ctx, owner, catalog, runtimeRole))

	// Workflow tables use ordinary data privileges, including columns that
	// previously required their own grant.
	exec(runtime, "UPDATE documents.document_event_heads SET content_version_id=content_version_id")
	// Reapplying grants removes older privileges at the owner boundary.
	exec(owner, "GRANT UPDATE(schema_version) ON documents.vault_metadata TO "+quotedRuntime)
	exec(owner, "GRANT CREATE ON SCHEMA documents TO "+quotedRuntime)
	exec(runtime, "UPDATE documents.vault_metadata SET schema_version=schema_version")
	exec(owner, "CREATE SCHEMA other; CREATE TABLE other.data (id INT); GRANT USAGE ON SCHEMA other TO "+quotedRuntime)
	exec(owner, "CREATE SCHEMA unrelated; CREATE TABLE unrelated.data (id INT)")

	require.NoError(t, GrantPostgresRuntime(ctx, owner, catalog, runtimeRole))
	require.NoError(t, GrantPostgresRuntime(ctx, owner, catalog, runtimeRole))
	exec(runtime, "UPDATE documents.document_event_heads SET published_at=published_at")
	exec(runtime, "CREATE TEMP TABLE scratch (id int)")
	for _, query := range []string{
		"UPDATE documents.vault_metadata SET schema_version=schema_version",
		"DELETE FROM documents.vault_metadata",
		"CREATE TABLE documents.forbidden (id int)",
		"SELECT * FROM unrelated.data",
	} {
		_, err := runtime.ExecContext(ctx, query)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		require.Equal(t, "42501", pgErr.Code)
	}
	_, err = runtime.ExecContext(ctx, "SELECT * FROM other.data")
	var denied *pgconn.PgError
	require.ErrorAs(t, err, &denied)
	require.Equal(t, "42501", denied.Code)
	exec(owner, "GRANT SELECT ON other.data TO "+quotedRuntime)
	require.NoError(t, GrantPostgresRuntime(ctx, owner, catalog, runtimeRole))
	exec(runtime, "SELECT * FROM other.data")

	// Use a restricted runtime connection that has real catalog privileges.
	require.ErrorContains(t, GrantPostgresRuntime(ctx, runtime, catalog, runtimeRole), "schema owner session")
	missing := PostgresCatalog{Schema: "missing", SchemaOwner: ownerRole}
	require.ErrorContains(t, GrantPostgresRuntime(ctx, owner, missing, runtimeRole), "schema does not exist")
	require.ErrorContains(t, GrantPostgresRuntime(ctx, owner, catalog, noLoginRole), "cannot log in")
	require.ErrorContains(t, GrantPostgresRuntime(ctx, owner, catalog, ownerRole), "separate runtime login")
}
