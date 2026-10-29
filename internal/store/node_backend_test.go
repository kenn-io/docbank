package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"uuid"

	"github.com/jackc/pgx/v5"
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
		"; GRANT SELECT ON "+schema+".audit_records,"+schema+".audit_authority,"+schema+".audit_scopes,"+schema+".audit_baselines,"+schema+".audit_memberships TO "+quotedRole+
		"; GRANT INSERT ON "+schema+".audit_records TO "+quotedRole+
		"; GRANT SELECT,UPDATE(high_water) ON "+schema+".node_allocation TO "+quotedRole)
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
	require.NoError(t, db.QueryRowContext(ctx, `SELECT id,(SELECT vault_uid FROM vault_metadata) FROM nodes WHERE parent_id IS NULL`).Scan(&s.rootID, &s.vaultID))
	return s
}

func TestNodeQueriesSQLite(t *testing.T) {
	t.Parallel()
	checkNodeQueries(t, newTestStore(t))
}

func TestNodeQueriesPostgres(t *testing.T) {
	t.Parallel()
	s := newPostgresQueryStore(t)
	checkNodeQueries(t, s)
	t.Run("directory snapshot", func(t *testing.T) { checkDirectorySnapshotPostgres(t, s) })
}

func checkDirectorySnapshotPostgres(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	const recordedAt = "2026-01-01T00:00:00.000000000Z"
	tx, err := s.writeDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	dirID, err := s.insertNodeTx(ctx, tx, s.RootID(), "snapshot-before", nodeKindDir, nil, recordedAt)
	require.NoError(t, err)
	childID, err := s.insertNodeTx(ctx, tx, dirID, "original", nodeKindDir, nil, recordedAt)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	// Reuse this test's database with a pgx tracer to commit after the first
	// SELECT. Every query still runs against PostgreSQL, including the real
	// DirectoryChildrenPage transaction and its subsequent path/count/page.
	conn, err := s.db.Conn(ctx)
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
	var writeErr error
	config.Tracer = &afterFirstSelect{run: func() {
		writeErr = s.withStorageTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `UPDATE nodes SET name='snapshot-after' WHERE id=$1`, dirID); err != nil {
				return err
			}
			_, err := s.insertNodeTx(ctx, tx, dirID, "added", nodeKindDir, nil, recordedAt)
			return err
		})
	}}
	readDB := stdlib.OpenDB(*config)
	t.Cleanup(func() { require.NoError(t, readDB.Close()) })
	reader := &Store{db: readDB, rootID: s.RootID()}
	page, err := reader.DirectoryChildrenPage(ctx, dirID, 10, 0)
	require.NoError(t, writeErr)
	require.NoError(t, err)
	require.Equal(t, "snapshot-before", page.Directory.Node.Name)
	require.Equal(t, "/snapshot-before", page.Directory.Path)
	require.Equal(t, 1, page.Total)
	require.Len(t, page.Children, 1)
	require.Equal(t, childID, page.Children[0].ID)

	after, err := s.DirectoryChildrenPage(ctx, dirID, 10, 0)
	require.NoError(t, err)
	require.Equal(t, "snapshot-after", after.Directory.Node.Name)
	require.Equal(t, "/snapshot-after", after.Directory.Path)
	require.Equal(t, 2, after.Total)
	require.Len(t, after.Children, 2)
	require.Equal(t, "added", after.Children[0].Name)
	require.Equal(t, childID, after.Children[1].ID)
}

type afterFirstSelect struct {
	once sync.Once
	run  func()
}

func (*afterFirstSelect) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (trace *afterFirstSelect) TraceQueryEnd(_ context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if data.Err == nil && data.CommandTag.Select() {
		trace.once.Do(trace.run)
	}
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
	dirID, err := s.insertNodeTx(ctx, tx, s.RootID(), "café", nodeKindDir, nil, recordedAt)
	require.NoError(t, err)
	fileID, err := s.insertNodeTx(ctx, tx, dirID, "report.pdf", nodeKindFile, new(versionID), recordedAt)
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
	t.Run("path queries", func(t *testing.T) { checkPathQueries(t, s, dir, file) })

	t.Run("canonical paths and directory pages", func(t *testing.T) {
		view, err := s.NodeViewByID(ctx, fileID)
		require.NoError(t, err)
		require.Equal(t, NodeView{Node: file, Path: "/café/report.pdf"}, view)
		rootPath, err := s.Path(ctx, s.RootID())
		require.NoError(t, err)
		require.Equal(t, "/", rootPath)

		tx, err := s.writeDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		folderID, err := s.insertNodeTx(ctx, tx, dirID, "z-folder", nodeKindDir, nil, recordedAt)
		require.NoError(t, err)
		trashID, err := s.insertNodeTx(ctx, tx, dirID, "discarded-folder", nodeKindDir, nil, recordedAt)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `UPDATE nodes SET trashed_at=$1 WHERE id=$2`, recordedAt, trashID)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())

		first, err := s.DirectoryChildrenPage(ctx, dirID, 1, 0)
		require.NoError(t, err)
		require.Equal(t, NodeView{Node: dir, Path: "/café"}, first.Directory)
		require.Equal(t, 2, first.Total)
		require.Len(t, first.Children, 1)
		require.Equal(t, folderID, first.Children[0].ID)
		last, total, err := s.ChildrenPage(ctx, dirID, 1, 1)
		require.NoError(t, err)
		require.Equal(t, 2, total)
		require.Equal(t, []Node{file}, last)
		empty, err := s.DirectoryChildrenPage(ctx, dirID, 1, 2)
		require.NoError(t, err)
		require.Equal(t, 2, empty.Total)
		require.Empty(t, empty.Children)

		_, err = s.DirectoryChildrenPage(ctx, fileID, 1, 0)
		require.ErrorIs(t, err, ErrNotDir)
		_, err = s.DirectoryChildrenPage(ctx, trashID, 1, 0)
		require.ErrorIs(t, err, ErrNotFound)
		_, err = s.NodeViewByID(ctx, 1<<62)
		require.ErrorIs(t, err, ErrNotFound)
		_, err = s.Path(ctx, 1<<62)
		require.ErrorIs(t, err, ErrNotFound)
	})
	t.Run("byte-ordered directory pages", func(t *testing.T) {
		var orderedDir int64
		err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
			var err error
			orderedDir, err = s.insertNodeTx(ctx, tx, s.RootID(), "ordered", nodeKindDir, nil, recordedAt)
			if err != nil {
				return err
			}
			for _, name := range []string{"b", "_x", "é", "Z", "a", "B"} {
				if _, err := s.insertNodeTx(ctx, tx, orderedDir, name, nodeKindDir, nil, recordedAt); err != nil {
					return err
				}
			}
			return nil
		})
		require.NoError(t, err)
		var names []string
		for offset := 0; offset < 6; offset += 2 {
			page, err := s.DirectoryChildrenPage(ctx, orderedDir, 2, offset)
			require.NoError(t, err)
			require.Equal(t, 6, page.Total)
			for _, child := range page.Children {
				names = append(names, child.Name)
			}
		}
		require.Equal(t, []string{"B", "Z", "_x", "a", "b", "é"}, names)
	})
	t.Run("node and path share a snapshot", func(t *testing.T) {
		tx, err := s.writeDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		parentID, err := s.insertNodeTx(ctx, tx, s.RootID(), "before", nodeKindDir, nil, recordedAt)
		require.NoError(t, err)
		nodeID, err := s.insertNodeTx(ctx, tx, parentID, "old", nodeKindDir, nil, recordedAt)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())

		// Commit on a separate connection after node resolution, before the
		// production nodeView reads its path. No timing race is required.
		view, err := s.nodeView(ctx, func(read *sql.Tx) (Node, error) {
			node, err := s.nodeByPath(ctx, read, "/before/old")
			if err != nil {
				return Node{}, err
			}
			_, err = s.writeDB.ExecContext(ctx, `UPDATE nodes
				SET name=CASE WHEN id=$1 THEN 'after' ELSE 'new' END
				WHERE id=$1 OR id=$2`, parentID, nodeID)
			return node, err
		})
		require.NoError(t, err)
		require.Equal(t, "old", view.Node.Name)
		require.Equal(t, "/before/old", view.Path)

		after, err := s.NodeViewByPath(ctx, "/after/new")
		require.NoError(t, err)
		require.Equal(t, "new", after.Node.Name)
		require.Equal(t, "/after/new", after.Path)
	})
	t.Run("rollback", func(t *testing.T) {
		tx, err := s.writeDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		id, err := s.insertNodeTx(ctx, tx, dirID, "discarded", nodeKindDir, nil, recordedAt)
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
		id, err := s.insertNodeTx(ctx, tx, dirID, "report.pdf", nodeKindDir, nil, recordedAt)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		live, err := s.ChildByName(ctx, dirID, "report.pdf")
		require.NoError(t, err)
		require.Equal(t, id, live.ID)
		require.NotEqual(t, fileID, live.ID)
	})
}
