package store

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func TestAuditHistoryPostgres(t *testing.T) {
	t.Parallel()
	source, reader, publish, scopeID, child, outside := newAuditHistoryPostgresFixture(t)
	publish()
	ctx := t.Context()

	first, err := reader.AuditHistory(ctx, child.ID, 2, "")
	require.NoError(t, err)
	require.Equal(t, "/Documents/renamed", first.Path)
	require.Equal(t, 3, first.Total)
	require.Len(t, first.Items, 2)
	require.Equal(t, "node_path", first.Items[0].Kind)
	require.Equal(t, "node_create", first.Items[1].Kind)
	require.NotNil(t, first.Items[0].OldPath)
	require.Equal(t, "/Documents/original", first.Items[0].OldPath.Path)
	require.NotEmpty(t, first.NextCursor)
	second, err := reader.AuditHistory(ctx, child.ID, 2, first.NextCursor)
	require.NoError(t, err)
	require.Len(t, second.Items, 1)
	require.Equal(t, "audit_inherit", second.Items[0].Kind)
	require.Empty(t, second.NextCursor)
	byPath, err := reader.AuditHistoryPath(ctx, first.Path, 2, "")
	require.NoError(t, err)
	require.Equal(t, first, byPath)
	_, err = reader.AuditHistory(ctx, outside.ID, 2, "")
	require.ErrorIs(t, err, ErrAuditNotEnrolled)

	// Pages cross both operation and event-ordinal boundaries. Compare with
	// history generated and read by the native SQLite store, including the
	// scope's baseline, chain head, member count and all canonical event fields.
	cursor := ""
	for page, kinds := range [][]string{{"node_path", "node_create"}, {"audit_inherit", "audit_enroll"}} {
		want, err := source.AuditScopeHistory(ctx, scopeID, 2, cursor)
		require.NoError(t, err)
		got, err := reader.AuditScopeHistory(ctx, scopeID, 2, cursor)
		require.NoError(t, err)
		require.Equal(t, want, got)
		require.Equal(t, 4, got.Total)
		require.Len(t, got.Items, 2)
		require.Equal(t, kinds[0], got.Items[0].Kind)
		require.Equal(t, kinds[1], got.Items[1].Kind)
		cursor = got.NextCursor
		if page == 0 {
			require.NotEmpty(t, cursor)
		}
	}
	require.Empty(t, cursor)
	_, err = reader.AuditScopeHistory(ctx, "11111111-1111-4111-8111-111111111111", 2, "")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestAuditHistorySnapshotPostgres(t *testing.T) {
	t.Parallel()
	for _, method := range []string{"node", "path", "scope"} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			source, reader, publish, scopeID, child, _ := newAuditHistoryPostgresFixture(t)
			publish()
			read := func(s *Store) (any, error) {
				switch method {
				case "node":
					return s.AuditHistory(t.Context(), child.ID, 2, "")
				case "path":
					return s.AuditHistoryPath(t.Context(), "/Documents/renamed", 2, "")
				default:
					return s.AuditScopeHistory(t.Context(), scopeID, 2, "")
				}
			}
			before, err := read(reader)
			require.NoError(t, err)

			conn, err := reader.db.Conn(t.Context())
			require.NoError(t, err)
			var config *pgx.ConnConfig
			err = conn.Raw(func(driverConn any) error {
				pgConn, ok := driverConn.(*stdlib.Conn)
				require.True(t, ok)
				config = pgConn.Conn().Config().Copy()
				return nil
			})
			require.NoError(t, conn.Close())
			require.NoError(t, err)
			config.Tracer = &afterFirstSelect{run: func() {
				// Restore the queried path after two real renames, so each
				// entry point can read both the old and the new history.
				moved, _, err := source.Move(t.Context(), child.ID, *child.ParentID, "temporary", child.Revision)
				require.NoError(t, err)
				_, _, err = source.Move(t.Context(), child.ID, *child.ParentID, child.Name, moved.Revision)
				require.NoError(t, err)
				publish()
			}}
			db := stdlib.OpenDB(*config)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			snapshot := &Store{db: db, rootID: reader.RootID(), postgresSchema: reader.postgresSchema}
			during, err := read(snapshot)
			require.NoError(t, err)
			require.Equal(t, before, during)
			after, err := read(reader)
			require.NoError(t, err)
			require.NotEqual(t, before, after)
			want, err := read(source)
			require.NoError(t, err)
			require.Equal(t, want, after)
		})
	}
}

// Build a reachable directory-only history through normal operations, then
// copy its catalogs to canonical PostgreSQL DDL. This is a read fixture, not
// a production import path or a replacement for full metadata validation.
func newAuditHistoryPostgresFixture(t *testing.T) (*Store, *Store, func(), string, Node, Node) {
	t.Helper()
	setup, owner := pgtest.NewDatabase(t)
	reader := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	source := newTestStore(t)
	require.NoError(t, reader.db.QueryRowContext(t.Context(), `SELECT vault_uid FROM vault_metadata`).Scan(&source.vaultID))
	_, err := source.writeDB.ExecContext(t.Context(), `UPDATE vault_metadata SET vault_uid=$1`, source.vaultID)
	require.NoError(t, err)
	directory, err := source.Mkdir(t.Context(), source.RootID(), "Documents")
	require.NoError(t, err)
	outside, err := source.Mkdir(t.Context(), source.RootID(), "Outside")
	require.NoError(t, err)
	plan, err := source.PreviewInitialAudit(t.Context(), directory.ID, "cli", nil)
	require.NoError(t, err)
	status, err := source.EnableInitialAudit(t.Context(), plan)
	require.NoError(t, err)
	child, err := source.Mkdir(t.Context(), directory.ID, "original")
	require.NoError(t, err)
	child, _, err = source.Move(t.Context(), child.ID, directory.ID, "renamed", child.Revision)
	require.NoError(t, err)
	return source, reader, func() { copyAuditHistoryFixture(t, source, setup) }, status.Scopes[0].ID, child, outside
}

func copyAuditHistoryFixture(t *testing.T, source *Store, setup *sql.DB) {
	t.Helper()
	ctx := t.Context()
	require.NoError(t, source.ValidateMetadata(ctx))
	from, err := source.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = from.Rollback() }()
	to, err := setup.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = to.Rollback() }()
	_, err = to.ExecContext(ctx, `SET LOCAL search_path=documents,pg_catalog`)
	require.NoError(t, err)
	tables := []string{"nodes", "audit_records", "audit_authority", "audit_scopes", "audit_baselines", "audit_memberships"}
	for _, table := range slices.Backward(tables) {
		_, err := to.ExecContext(ctx, "DELETE FROM "+table)
		require.NoError(t, err)
	}
	for _, table := range tables {
		query := "SELECT * FROM " + table
		if table == "nodes" {
			query += " ORDER BY id"
		}
		rows, err := from.QueryContext(ctx, query)
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		columns, err := rows.Columns()
		require.NoError(t, err)
		values, destinations := make([]any, len(columns)), make([]any, len(columns))
		parameters := make([]string, len(columns))
		for i := range columns {
			destinations[i] = &values[i]
			parameters[i] = fmt.Sprintf("$%d", i+1)
		}
		insert := "INSERT INTO " + table + "(" + strings.Join(columns, ",") + ") VALUES(" + strings.Join(parameters, ",") + ")"
		for rows.Next() {
			require.NoError(t, rows.Scan(destinations...))
			_, err := to.ExecContext(ctx, insert, values...)
			require.NoError(t, err)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
	}
	var highWater int64
	require.NoError(t, from.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name='nodes'`).Scan(&highWater))
	_, err = to.ExecContext(ctx, `UPDATE node_allocation SET high_water=$1 WHERE singleton=1`, highWater)
	require.NoError(t, err)
	require.NoError(t, to.Commit())
}
