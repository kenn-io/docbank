package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

// Queries use setup-owned DDL and an explicitly granted runtime login. This
// constructs the qualified store surface; complete PostgreSQL vaults are not openable.
func newPostgresQueryStore(t *testing.T) *Store {
	t.Helper()
	setup, owner := pgtest.NewDatabase(t)
	return newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
}

func newPostgresSchemaStore(t *testing.T, setup *sql.DB, cfg pgschema.Config) *Store {
	t.Helper()
	ctx := t.Context()
	id, err := pgschema.Setup(ctx, setup, cfg)
	require.NoError(t, err)

	role := fmt.Sprintf("query_runtime_%x", uuid.New())
	quotedRole := pgx.Identifier{role}.Sanitize()
	_, err = setup.ExecContext(ctx, "CREATE ROLE "+quotedRole+" LOGIN PASSWORD 'fixture-query-only'")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := setup.ExecContext(context.Background(), "DROP OWNED BY "+quotedRole+"; DROP ROLE "+quotedRole)
		require.NoError(t, err)
	})
	schema := pgx.Identifier{cfg.Schema}.Sanitize()
	_, err = setup.ExecContext(ctx, "GRANT USAGE ON SCHEMA "+schema+" TO "+quotedRole+
		"; GRANT SELECT ON "+schema+".vault_metadata TO "+quotedRole+
		"; GRANT SELECT,INSERT,UPDATE,DELETE ON "+schema+".nodes,"+schema+".blobs,"+schema+".content_versions TO "+quotedRole+
		"; GRANT SELECT,INSERT ON "+schema+".blob_checksums TO "+quotedRole+
		"; GRANT USAGE ON SEQUENCE "+schema+".nodes_id_seq TO "+quotedRole)
	require.NoError(t, err)
	config, err := pgx.ParseConfig(os.Getenv("DOCBANK_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	require.NoError(t, setup.QueryRowContext(ctx, `SELECT current_database()`).Scan(&config.Database))
	config.User, config.Password = role, "fixture-query-only"
	config.RuntimeParams["search_path"] = schema + ",pg_catalog"
	db := stdlib.OpenDB(*config)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	actual, err := pgschema.Validate(ctx, db, cfg)
	require.NoError(t, err)
	require.Equal(t, id, actual)
	s := &Store{db: db, writeDB: db, postgresSchema: cfg.Schema}
	require.NoError(t, db.QueryRowContext(ctx, `SELECT id FROM nodes WHERE parent_id IS NULL`).Scan(&s.rootID))
	return s
}

func TestNodeQueriesSQLite(t *testing.T) {
	t.Parallel()
	checkNodeQueries(t, newTestStore(t))
}

func TestNodeQueriesPostgres(t *testing.T) {
	t.Parallel()
	s := newPostgresQueryStore(t)
	// Ordinary writes must not bypass allocation; restoring IDs needs an
	// explicit import path that also restores the sequence high-water mark.
	_, err := s.writeDB.ExecContext(t.Context(), `INSERT INTO nodes(id,parent_id,name,kind,created_at,modified_at)
		VALUES(1000,$1,'imported','dir',$2,$2)`, s.RootID(), "2026-01-01T00:00:00.000000000Z")
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, "428C9", pgErr.Code) // generated_always
	checkNodeQueries(t, s)
}

func checkNodeQueries(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	const recordedAt = "2026-01-01T00:00:00.000000000Z"
	const versionID = "11111111-1111-4111-8111-111111111111"
	const operationID = "22222222-2222-4222-8222-222222222222"
	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const md5 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	tx, err := s.writeDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	dirID, err := insertNodeTx(ctx, tx, s.RootID(), "café", nodeKindDir, nil, recordedAt)
	require.NoError(t, err)
	fileID, err := insertNodeTx(ctx, tx, dirID, "report.pdf", nodeKindFile, new(versionID), recordedAt)
	require.NoError(t, err)
	require.NotEqual(t, dirID, fileID)
	_, err = tx.ExecContext(ctx, `INSERT INTO blobs(hash, size, created_at) VALUES ($1, 17, $2)`, hash, recordedAt)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO blob_checksums(blob_sha256, md5) VALUES ($1, $2)`, hash, md5)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO content_versions
		(version_id, node_id, blob_hash, size, mime_type, recorded_at, node_revision, introduced_operation_id, transition_kind)
		VALUES ($1, $2, $3, 17, 'application/pdf', $4, 1, $5, 'content_create')`,
		versionID, fileID, hash, recordedAt, operationID)
	require.NoError(t, err)
	inside, err := nodeByIDTx(tx, fileID)
	require.NoError(t, err)
	require.Equal(t, fileID, inside.ID)
	require.NoError(t, tx.Commit())

	dir, err := s.ChildByName(ctx, s.RootID(), "cafe\u0301")
	require.NoError(t, err)
	require.Equal(t, Node{ID: dirID, ParentID: new(s.RootID()), Name: "café", Kind: nodeKindDir,
		Revision: 1, CreatedAt: recordedAt, ModifiedAt: recordedAt}, dir)
	file, err := s.NodeByID(ctx, fileID)
	require.NoError(t, err)
	require.Equal(t, Node{ID: fileID, ParentID: new(dirID), Name: "report.pdf", Kind: nodeKindFile,
		CurrentVersionID: versionID, BlobHash: hash, MD5: md5, Size: 17, MimeType: "application/pdf",
		Revision: 1, CreatedAt: recordedAt, ModifiedAt: recordedAt}, file)

	t.Run("rollback", func(t *testing.T) {
		tx, err := s.writeDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		id, err := insertNodeTx(ctx, tx, dirID, "discarded", nodeKindDir, nil, recordedAt)
		require.NoError(t, err)
		require.NoError(t, tx.Rollback())
		_, err = s.NodeByID(ctx, id)
		require.ErrorIs(t, err, ErrNotFound)
		_, err = s.ChildByName(ctx, dirID, "discarded")
		require.ErrorIs(t, err, ErrNotFound)
	})
	t.Run("trashed name can be reused", func(t *testing.T) {
		_, err := s.writeDB.ExecContext(ctx, `UPDATE nodes SET trashed_at=$1 WHERE id=$2`, recordedAt, fileID)
		require.NoError(t, err)
		_, err = s.ChildByName(ctx, dirID, "report.pdf")
		require.ErrorIs(t, err, ErrNotFound)
		trashed, err := s.NodeByID(ctx, fileID)
		require.NoError(t, err)
		require.Equal(t, new(recordedAt), trashed.TrashedAt)
		tx, err := s.writeDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		id, err := insertNodeTx(ctx, tx, dirID, "report.pdf", nodeKindDir, nil, recordedAt)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		live, err := s.ChildByName(ctx, dirID, "report.pdf")
		require.NoError(t, err)
		require.Equal(t, id, live.ID)
		require.NotEqual(t, fileID, live.ID)
	})
}
