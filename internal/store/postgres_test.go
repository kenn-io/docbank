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
	_, err := pgschema.Setup(t.Context(), setup, pgschema.Config{Schema: "other", SchemaOwner: owner})
	require.NoError(t, err)
	connection.RuntimeParams["search_path"] = "other,pg_catalog"
	s, err := OpenPostgres(t.Context(), connection, cfg)
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
	_, err := pgschema.Setup(t.Context(), setup, cfg)
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
	s, err := OpenPostgres(t.Context(), connection, cfg)
	require.Error(t, err)
	require.Nil(t, s)
}
