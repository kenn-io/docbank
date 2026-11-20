package store

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/pgstoretest"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func TestPostgresPoolBindingPostgres(t *testing.T) {
	t.Parallel()
	setup, owner := pgtest.NewDatabase(t)
	cfg := pgschema.Config{Schema: "documents", SchemaOwner: owner}
	connection := pgstoretest.RuntimeConfig(t, setup, cfg)
	release := pgtest.ReserveSchemaInstall(t)
	_, err := pgschema.Setup(t.Context(), setup, pgschema.Config{Schema: "other", SchemaOwner: owner})
	release()
	require.NoError(t, err)
	connection.RuntimeParams["search_path"] = "other,pg_catalog"
	s, err := OpenPostgres(t.Context(), connection, cfg, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	first, err := s.db.Conn(t.Context())
	require.NoError(t, err)
	defer func() { _ = first.Close() }()
	second, err := s.db.Conn(t.Context())
	require.NoError(t, err)
	defer func() { _ = second.Close() }()
	for _, conn := range []*sql.Conn{first, second} {
		var schema string
		require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT current_schema()`).Scan(&schema))
		require.Equal(t, "documents", schema)
	}
	require.NoError(t, first.Close())
	require.NoError(t, second.Close())
	s.db.SetMaxIdleConns(0)
	_, err = s.Mkdir(t.Context(), s.RootID(), "bound")
	require.NoError(t, err)
	var wrong int
	require.NoError(t, setup.QueryRowContext(t.Context(), `SELECT count(*) FROM other.nodes WHERE name='bound'`).Scan(&wrong))
	require.Zero(t, wrong)
	var right int
	require.NoError(t, setup.QueryRowContext(t.Context(), `SELECT count(*) FROM documents.nodes WHERE name='bound'`).Scan(&right))
	require.Equal(t, 1, right)
}

func TestPostgresOpenerRefusesOwnerPostgres(t *testing.T) {
	t.Parallel()
	setup, owner := pgtest.NewDatabase(t)
	cfg := pgschema.Config{Schema: "documents", SchemaOwner: owner}
	release := pgtest.ReserveSchemaInstall(t)
	_, err := pgschema.Setup(t.Context(), setup, cfg)
	release()
	require.NoError(t, err)
	var connection *pgx.ConnConfig
	conn, err := setup.Conn(t.Context())
	require.NoError(t, err)
	require.NoError(t, conn.Raw(func(raw any) error {
		pgConn, ok := raw.(*stdlib.Conn)
		if !ok {
			return fmt.Errorf("unexpected postgres driver %T", raw)
		}
		connection = pgConn.Conn().Config().Copy()
		return nil
	}))
	require.NoError(t, conn.Close())
	s, err := OpenPostgres(t.Context(), connection, cfg, t.TempDir())
	require.Error(t, err)
	require.Nil(t, s)
}

func TestPostgresLaneControlsAreVaultLocalPostgres(t *testing.T) {
	// Isolate the working directory in case the store uses it for lane controls.
	t.Chdir(t.TempDir())
	first := newPostgresQueryStore(t)
	second := newPostgresQueryStore(t)
	ctx := t.Context()
	lane := StorageOperationKindPhotoImport

	_, err := first.SetLaneControl(ctx, LaneControl{Lane: lane, Paused: true, Concurrency: 1}, 1)
	require.NoError(t, err)
	firstControl, err := first.LaneControl(ctx, lane)
	require.NoError(t, err)
	require.True(t, firstControl.Paused)
	require.EqualValues(t, 2, firstControl.Revision)

	secondControl, err := second.LaneControl(ctx, lane)
	require.NoError(t, err)
	require.False(t, secondControl.Paused, "pausing one vault must not pause another vault")
	require.EqualValues(t, 1, secondControl.Revision)
}

// Template copies stand in for separate installations, so a copy must not
// share the identities that pgschema.Setup chose for the template.
func TestTemplateCopiesHaveTheirOwnIdentitiesPostgres(t *testing.T) {
	t.Parallel()
	identities := func(setup *sql.DB) []string {
		var vault, epoch, incarnation, current, store, ownership string
		require.NoError(t, setup.QueryRowContext(t.Context(), `SELECT
			(SELECT vault_uid::text FROM documents.vault_metadata),
			(SELECT epoch FROM documents.vault_change_head),
			(SELECT incarnation_id FROM documents.processing_incarnations),
			(SELECT incarnation_id FROM documents.current_processing_incarnation),
			(SELECT store_id FROM documents.blob_stores),
			(SELECT ownership_epoch FROM documents.blob_stores)`).Scan(&vault, &epoch, &incarnation, &current, &store, &ownership))
		require.Equal(t, incarnation, current)
		return []string{vault, epoch, incarnation, store, ownership}
	}
	first, _, _ := pgstoretest.NewDatabase(t)
	second, _, _ := pgstoretest.NewDatabase(t)
	a, b := identities(first), identities(second)
	for i := range a {
		require.NotEqual(t, a[i], b[i], "identity %d", i)
	}
}

// renewIdentities renews the tables in which Setup writes rows. A new table
// that Setup fills needs a renewal there too, or every copy shares its rows.
func TestSetupFillsOnlyRenewedTablesPostgres(t *testing.T) {
	t.Parallel()
	setup, owner := pgtest.NewDatabase(t)
	release := pgtest.ReserveSchemaInstall(t)
	_, err := pgschema.Setup(t.Context(), setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	release()
	require.NoError(t, err)
	rows, err := setup.QueryContext(t.Context(), `SELECT table_name FROM information_schema.tables
		WHERE table_schema='documents' AND table_type='BASE TABLE' ORDER BY table_name`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var tables []string
	for rows.Next() {
		var table string
		require.NoError(t, rows.Scan(&table))
		tables = append(tables, table)
	}
	require.NoError(t, rows.Err())
	var filled []string
	for _, table := range tables {
		var exists bool
		require.NoError(t, setup.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM documents.`+pgx.Identifier{table}.Sanitize()+`)`).Scan(&exists))
		if exists {
			filled = append(filled, table)
		}
	}
	require.Equal(t, pgstoretest.InitializedTables, filled)
}
