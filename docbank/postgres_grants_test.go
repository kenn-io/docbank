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
	_, err := InitializePostgres(ctx, owner, catalog, filepath.Join(t.TempDir(), "vault"))
	require.NoError(t, err)
	require.ErrorContains(t, GrantPostgresRuntime(ctx, owner, catalog, runtimeRole), "TEMPORARY")
	exec(admin, "GRANT TEMPORARY ON DATABASE "+quotedDatabase+" TO "+quotedRuntime)
	require.NoError(t, GrantPostgresRuntime(ctx, owner, catalog, runtimeRole))

	// Simulate privileges narrower in this build than in an earlier install.
	exec(owner, "GRANT UPDATE ON documents.vault_metadata TO "+quotedRuntime)
	exec(owner, "GRANT UPDATE(content_version_id) ON documents.document_event_heads TO "+quotedRuntime)
	exec(owner, "GRANT CREATE ON SCHEMA documents TO "+quotedRuntime)
	exec(runtime, "UPDATE documents.vault_metadata SET schema_version=schema_version")
	exec(runtime, "UPDATE documents.document_event_heads SET content_version_id=content_version_id")
	exec(owner, "CREATE SCHEMA other; GRANT USAGE ON SCHEMA other TO "+quotedRuntime)

	require.NoError(t, GrantPostgresRuntime(ctx, owner, catalog, runtimeRole))
	require.NoError(t, GrantPostgresRuntime(ctx, owner, catalog, runtimeRole))
	exec(runtime, "UPDATE documents.document_event_heads SET published_at=published_at")
	exec(runtime, "CREATE TEMP TABLE scratch (id int)")
	for _, query := range []string{
		"UPDATE documents.vault_metadata SET schema_version=schema_version",
		"UPDATE documents.document_event_heads SET content_version_id=content_version_id",
		"CREATE TABLE documents.forbidden (id int)",
	} {
		_, err := runtime.ExecContext(ctx, query)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		require.Equal(t, "42501", pgErr.Code)
	}
	var otherUsage bool
	require.NoError(t, runtime.QueryRowContext(ctx, "SELECT has_schema_privilege('other','USAGE')").Scan(&otherUsage))
	require.True(t, otherUsage)

	// Use a restricted runtime connection that has real catalog privileges.
	require.ErrorContains(t, GrantPostgresRuntime(ctx, runtime, catalog, runtimeRole), "schema owner session")
	missing := PostgresCatalog{Schema: "missing", SchemaOwner: ownerRole}
	require.ErrorContains(t, GrantPostgresRuntime(ctx, owner, missing, runtimeRole), "schema does not exist")
	require.ErrorContains(t, GrantPostgresRuntime(ctx, owner, catalog, noLoginRole), "cannot log in")
	require.ErrorContains(t, GrantPostgresRuntime(ctx, owner, catalog, ownerRole), "separate runtime login")
}
