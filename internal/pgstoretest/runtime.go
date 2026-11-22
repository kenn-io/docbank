// Package pgstoretest shares canonical Store runtime fixtures across PostgreSQL tests.
package pgstoretest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
	"go.kenn.io/docbank/internal/store/schemaformat"
)

// runtimePassword is the password of every runtime test login.
const runtimePassword = "fixture-query-only"

// createRuntimeLogin creates a runtime test login. The server and the client
// each derive a key from the password on every connection, as often as the
// stored verifier says; tests open thousands of connections. The password is
// a constant in this file, so one iteration loses no protection.
// SET LOCAL ends with the statement's implicit transaction.
func createRuntimeLogin(quotedRole string) string {
	return "SET LOCAL scram_iterations = 1; CREATE ROLE " + quotedRole + " LOGIN PASSWORD '" + runtimePassword + "'"
}

// templateSchema is the schema that NewDatabase installs.
const templateSchema = "documents"

// RuntimeConfig installs canonical DDL and grants a separate test login the
// qualified Store operations. The caller opens and closes its runtime pool.
func RuntimeConfig(tb testing.TB, setup *sql.DB, cfg pgschema.Config) *pgx.ConnConfig {
	tb.Helper()
	ctx := tb.Context()
	// Setup only validates a schema that is already installed, such as one a
	// metadata import created, and that takes no installation slot.
	var installed bool
	require.NoError(tb, setup.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)`, cfg.Schema).Scan(&installed))
	release := func() {}
	if !installed {
		release = pgtest.ReserveSchemaInstall(tb)
	}
	_, err := pgschema.Setup(ctx, setup, cfg)
	release()
	require.NoError(tb, err)

	role := "query_runtime_" + strings.ReplaceAll(uuid.New().String(), "-", "")
	quotedRole := pgx.Identifier{role}.Sanitize()
	_, err = setup.ExecContext(ctx, createRuntimeLogin(quotedRole))
	require.NoError(tb, err)
	tb.Cleanup(func() {
		_, err := setup.ExecContext(context.WithoutCancel(tb.Context()), "DROP OWNED BY "+quotedRole+"; DROP ROLE "+quotedRole)
		require.NoError(tb, err)
	})
	require.NoError(tb, pgschema.GrantRuntime(ctx, setup, cfg, role))
	var database string
	require.NoError(tb, setup.QueryRowContext(ctx, `SELECT current_database()`).Scan(&database))
	return runtimeConfig(tb, database, cfg.Schema, role)
}

// NewDatabase creates a database that already holds the canonical schema and
// grants a separate test login the qualified Store operations, as
// RuntimeConfig would. It returns the owner's pool, the schema settings and
// the runtime login's settings; the caller opens and closes its runtime pool.
//
// The database is a copy of a template that the package run builds once, so a
// test neither runs the schema's DDL nor waits for a schema installation
// slot. Each copy gets new vault identities, as a separate installation
// would. The runtime login belongs to the template and its copies; grants
// that a test revokes stay inside the test's own database.
func NewDatabase(tb testing.TB) (*sql.DB, pgschema.Config, *pgx.ConnConfig) {
	tb.Helper()
	server := pgtest.ServerConfig(tb)
	name, role, err := storeTemplate(tb.Context(), tb, server)
	require.NoError(tb, err)
	setup, owner := pgtest.CopyDatabase(tb, name)
	require.NoError(tb, renewIdentities(tb.Context(), setup))
	var database string
	require.NoError(tb, setup.QueryRowContext(tb.Context(), `SELECT current_database()`).Scan(&database))
	return setup, pgschema.Config{Schema: templateSchema, SchemaOwner: owner}, runtimeConfig(tb, database, templateSchema, role)
}

// InitializedTables lists the tables in which pgschema.Setup writes rows for
// a new vault. renewIdentities gives each copy of the template new values for
// the identities and times in them.
var InitializedTables = []string{
	"blob_stores", "current_processing_incarnation", "document_people_state", "node_allocation",
	"nodes", "processing_incarnations", "vault_change_head", "vault_metadata",
}

// renewIdentities replaces the identities and times that pgschema.Setup chose
// for the template with new ones, as Setup would choose for another vault.
func renewIdentities(ctx context.Context, setup *sql.DB) error {
	tx, err := setup.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("renew the copied vault identities: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // A committed transaction ignores it.
	now := time.Now().UTC().Format(schemaformat.TimestampLayout)
	incarnation := uuid.New().String()
	schema := pgx.Identifier{templateSchema}.Sanitize()
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		// The processing incarnation is a key that other tables reference.
		{`SET CONSTRAINTS ALL DEFERRED`, nil},
		{`UPDATE ` + schema + `.vault_metadata SET vault_uid=$1`, []any{uuid.New()}},
		{`UPDATE ` + schema + `.vault_change_head SET epoch=$1`, []any{uuid.New().String()}},
		{`UPDATE ` + schema + `.nodes SET created_at=$1, modified_at=$1`, []any{now}},
		{`UPDATE ` + schema + `.document_people_state SET updated_at=$1`, []any{now}},
		{`UPDATE ` + schema + `.processing_incarnations SET incarnation_id=$1, created_at=$2`, []any{incarnation, now}},
		{`UPDATE ` + schema + `.current_processing_incarnation SET incarnation_id=$1`, []any{incarnation}},
		{`UPDATE ` + schema + `.blob_stores SET store_id=$1, ownership_epoch=$2, created_at=$3`, []any{uuid.New().String(), uuid.New().String(), now}},
	} {
		if _, err := tx.ExecContext(ctx, statement.sql, statement.args...); err != nil {
			return fmt.Errorf("renew the copied vault identities: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("renew the copied vault identities: %w", err)
	}
	return nil
}

func runtimeConfig(tb testing.TB, database, schema, role string) *pgx.ConnConfig {
	tb.Helper()
	config := pgtest.ServerConfig(tb)
	config.Database = database
	config.User, config.Password = role, runtimePassword
	config.RuntimeParams["search_path"] = pgx.Identifier{schema}.Sanitize() + ",pg_catalog"
	return config
}

// templateNameEnv names the Store template that the processes of one
// package run share. The process that starts the run chooses the name, and
// the children that packagetest.RunShards starts inherit it.
const templateNameEnv = "DOCBANK_TEST_STORE_TEMPLATE"

// templateState is the package run's Store template database and its runtime
// login. The process that chose their names drops them.
type templateState struct {
	mu    sync.Mutex
	name  string
	role  string
	ready bool
	owner bool
}

var template = newTemplateState()

func newTemplateState() *templateState {
	t := &templateState{}
	suffix := os.Getenv(templateNameEnv)
	if suffix == "" {
		suffix = strings.ReplaceAll(uuid.New().String(), "-", "")
		if err := os.Setenv(templateNameEnv, suffix); err != nil {
			panic(err)
		}
		t.owner = true
	}
	t.name, t.role = "docbank_template_"+suffix, "query_runtime_"+suffix
	return t
}

// storeTemplate returns the template, building it if no process of the run
// has yet. The processes take turns under an advisory lock, so one of them
// builds the template and the others find it. A build ends by marking the
// database as a template, so a database left unmarked by a failed build, or
// by a process that died during one, is dropped and built again.
func storeTemplate(ctx context.Context, tb testing.TB, server *pgx.ConnConfig) (name, role string, err error) {
	tb.Helper()
	template.mu.Lock()
	defer template.mu.Unlock()
	if template.ready {
		return template.name, template.role, nil
	}
	conn, err := pgx.ConnectConfig(ctx, server)
	if err != nil {
		return "", "", fmt.Errorf("connect to build the Store template: %w", err)
	}
	// Closing the session releases its advisory lock.
	defer func() { err = errors.Join(err, conn.Close(context.WithoutCancel(ctx))) }()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, template.name); err != nil {
		return "", "", fmt.Errorf("wait for the Store template: %w", err)
	}
	var built bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1 AND datistemplate)`, template.name).Scan(&built); err != nil {
		return "", "", fmt.Errorf("look for the Store template: %w", err)
	}
	if !built {
		if err := dropTemplate(ctx, conn); err != nil {
			return "", "", fmt.Errorf("drop an unfinished Store template: %w", err)
		}
		if err := createTemplate(ctx, tb, conn, server); err != nil {
			return "", "", err
		}
	}
	template.ready = true
	return template.name, template.role, nil
}

func createTemplate(ctx context.Context, tb testing.TB, conn *pgx.Conn, server *pgx.ConnConfig) error {
	tb.Helper()
	quotedName := pgx.Identifier{template.name}.Sanitize()
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+quotedName); err != nil {
		return fmt.Errorf("create the Store template database: %w", err)
	}
	err := buildTemplate(ctx, tb, server, template.name, template.role)
	if err == nil {
		if _, markErr := conn.Exec(ctx, "ALTER DATABASE "+quotedName+" IS_TEMPLATE true"); markErr != nil {
			err = fmt.Errorf("mark the Store template as built: %w", markErr)
		}
	}
	if err != nil {
		return errors.Join(err, dropTemplate(context.WithoutCancel(ctx), conn))
	}
	return nil
}

func buildTemplate(ctx context.Context, tb testing.TB, server *pgx.ConnConfig, name, role string) (err error) {
	tb.Helper()
	config := server.Copy()
	config.Database = name
	setup := stdlib.OpenDB(*config)
	// A database can be copied only while no session is connected to it.
	defer func() { err = errors.Join(err, setup.Close()) }()
	release := pgtest.ReserveSchemaInstall(tb)
	catalog := pgschema.Config{Schema: templateSchema, SchemaOwner: server.User}
	_, err = pgschema.Setup(ctx, setup, catalog)
	release()
	if err != nil {
		return fmt.Errorf("install the Store schema in the template: %w", err)
	}
	if _, err := setup.ExecContext(ctx, createRuntimeLogin(pgx.Identifier{role}.Sanitize())); err != nil {
		return fmt.Errorf("create the template's runtime login: %w", err)
	}
	if err := pgschema.GrantRuntime(ctx, setup, catalog, role); err != nil {
		return fmt.Errorf("grant the template's runtime login: %w", err)
	}
	return nil
}

// dropTemplate drops the Store template database and its runtime login if
// they exist. PostgreSQL refuses to drop a database marked as a template, so
// the mark comes off first.
func dropTemplate(ctx context.Context, conn *pgx.Conn) error {
	quotedName := pgx.Identifier{template.name}.Sanitize()
	var marked bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1 AND datistemplate)`, template.name).Scan(&marked); err != nil {
		return fmt.Errorf("look for the Store template: %w", err)
	}
	if marked {
		if _, err := conn.Exec(ctx, "ALTER DATABASE "+quotedName+" IS_TEMPLATE false"); err != nil {
			return fmt.Errorf("unmark the Store template: %w", err)
		}
	}
	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+quotedName+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("drop the Store template database: %w", err)
	}
	if _, err := conn.Exec(ctx, "DROP ROLE IF EXISTS "+pgx.Identifier{template.role}.Sanitize()); err != nil {
		return fmt.Errorf("drop the Store template's runtime login: %w", err)
	}
	return nil
}

// DropTemplate drops the package run's Store template and its runtime login
// if this process chose their names and a process built them. Each package
// that calls NewDatabase calls it from TestMain after its tests, when every
// copy has been dropped.
func DropTemplate(ctx context.Context) error {
	template.mu.Lock()
	defer template.mu.Unlock()
	dsn := os.Getenv("DOCBANK_TEST_POSTGRES_DSN")
	if !template.owner || dsn == "" {
		return nil
	}
	server, err := pgx.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("drop the Store template: %w", err)
	}
	conn, err := pgx.ConnectConfig(ctx, server)
	if err != nil {
		return fmt.Errorf("connect to drop the Store template %s: %w", template.name, err)
	}
	if err := errors.Join(dropTemplate(ctx, conn), conn.Close(context.WithoutCancel(ctx))); err != nil {
		return fmt.Errorf("drop the Store template %s: %w", template.name, err)
	}
	template.ready = false
	return nil
}

// Finish drops the process's test databases once its tests have run, and
// the run's Store template in the process that named it. It returns code, or
// 1 when a drop fails. TestMain passes it the tests' exit code.
func Finish(code int) int {
	code = pgtest.Finish(code)
	// TestMain has no caller context.
	if err := DropTemplate(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return code
}
