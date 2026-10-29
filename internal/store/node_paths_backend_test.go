package store

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

// Run through the same core catalog fixture on SQLite and PostgreSQL. Complete
// mutation APIs are not yet available on PostgreSQL.
func checkPathQueries(t *testing.T, s *Store, directory, file Node) {
	t.Helper()
	ctx := t.Context()
	for _, path := range []string{"/café/report.pdf", "//cafe\u0301//report.pdf/", "café/report.pdf"} {
		got, err := s.NodeByPath(ctx, path)
		require.NoError(t, err)
		require.Equal(t, file, got)
	}
	for _, path := range []string{"", "/", "///"} {
		got, err := s.NodeByPath(ctx, path)
		require.NoError(t, err)
		require.Equal(t, s.RootID(), got.ID)
	}
	for _, tc := range []struct {
		path string
		want error
	}{
		{"/Café/report.pdf", ErrNotFound},
		{"/café/missing/report.pdf", ErrNotFound},
		{"/café/report.pdf/child", ErrNotFound},
		{"/café/report.pdf/..", ErrInvalidName},
		{"/café/missing/..", ErrNotFound},
		{"/café/\x00", ErrInvalidName},
	} {
		_, err := s.NodeByPath(ctx, tc.path)
		require.ErrorIs(t, err, tc.want, tc.path)
	}

	tx, err := s.writeDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	const recordedAt = "2026-01-01T00:00:00.000000000Z"
	const name = `資料 "notes"`
	nestedID, err := s.insertNodeTx(ctx, tx, directory.ID, name, nodeKindDir, nil, recordedAt)
	require.NoError(t, err)
	leafID, err := s.insertNodeTx(ctx, tx, nestedID, "leaf", nodeKindDir, nil, recordedAt)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	view, err := s.NodeViewByPath(ctx, `/café/資料 "notes"/leaf`)
	require.NoError(t, err)
	require.Equal(t, leafID, view.Node.ID)
	require.Equal(t, `/café/資料 "notes"/leaf`, view.Path)

	ids := []int64{leafID, s.RootID(), file.ID, directory.ID, leafID}
	wantPaths := []string{`/café/資料 "notes"/leaf`, "/", "/café/report.pdf", "/café", `/café/資料 "notes"/leaf`}
	paths, err := s.pathsOf(ctx, s.db, ids)
	require.NoError(t, err)
	require.Equal(t, wantPaths, paths)
	_, err = s.pathsOf(ctx, s.db, []int64{leafID, 1 << 62, file.ID})
	require.ErrorIs(t, err, ErrNotFound)

	read, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = read.Rollback() }()
	before, err := s.nodeByPath(ctx, read, `/café/資料 "notes"/leaf`)
	require.NoError(t, err)
	_, err = s.writeDB.ExecContext(ctx, `UPDATE nodes SET name='renamed' WHERE id=$1`, nestedID)
	require.NoError(t, err)
	retained, err := s.nodeByPath(ctx, read, `/café/資料 "notes"/leaf`)
	require.NoError(t, err)
	require.Equal(t, before, retained)
	paths, err = s.pathsOf(ctx, read, ids)
	require.NoError(t, err)
	require.Equal(t, wantPaths, paths)
	require.NoError(t, read.Commit())
	_, err = s.NodeByPath(ctx, `/café/資料 "notes"/leaf`)
	require.ErrorIs(t, err, ErrNotFound)
	paths, err = s.pathsOf(ctx, s.db, []int64{leafID, leafID})
	require.NoError(t, err)
	require.Equal(t, []string{"/café/renamed/leaf", "/café/renamed/leaf"}, paths)

	// Match trashNodeTx's topology: stamp the whole subtree, record the top
	// node's origin, then detach it. Display paths still describe retained nodes.
	trashTx, err := s.writeDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = trashTx.Rollback() }()
	_, err = trashTx.ExecContext(ctx, `UPDATE nodes SET trashed_at=$1 WHERE id=$2 OR id=$3`,
		recordedAt, nestedID, leafID)
	require.NoError(t, err)
	_, err = trashTx.ExecContext(ctx, `UPDATE nodes SET parent_id=$1,trash_parent=$2,trash_name=name WHERE id=$3`,
		s.RootID(), directory.ID, nestedID)
	require.NoError(t, err)
	require.NoError(t, trashTx.Commit())
	_, err = s.NodeByPath(ctx, "/renamed/leaf")
	require.ErrorIs(t, err, ErrNotFound)
	paths, err = s.pathsOf(ctx, s.db, []int64{nestedID, leafID})
	require.NoError(t, err)
	require.Equal(t, []string{"/renamed", "/renamed/leaf"}, paths)
}
