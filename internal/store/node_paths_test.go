package store

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNodeByPathResolution(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	directory, err := s.MkdirAll(ctx, "/café/資料")
	require.NoError(t, err)
	file, err := s.CreateFile(ctx, directory.ID, "report.txt", fakeHash("a1"), 17, "text/plain")
	require.NoError(t, err)
	for _, path := range []string{"/café/資料/report.txt", "//cafe\u0301//資料/report.txt/", "café/資料/report.txt"} {
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
		{"/Café/資料/report.txt", ErrNotFound},
		{"/café/missing/report.txt", ErrNotFound},
		{"/café/資料/report.txt/child", ErrNotFound},
		{"/café/../資料", ErrInvalidName},
		{"/café/資料/\x00", ErrInvalidName},
		{"/café/資料/\xff", ErrInvalidName},
		{"/missing/..", ErrNotFound},
		{"/./missing", ErrInvalidName},
	} {
		_, err := s.NodeByPath(ctx, tc.path)
		require.ErrorIs(t, err, tc.want, tc.path)
	}
	_, _, err = s.Trash(ctx, directory.ID, UnconditionalRev)
	require.NoError(t, err)
	_, err = s.NodeByPath(ctx, "/café/資料/report.txt")
	require.ErrorIs(t, err, ErrNotFound)
	// Trash roots are reparented to the vault root but must stay unresolvable.
	_, err = s.NodeByPath(ctx, "/資料/report.txt")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestNodeByPathUsesCallerSnapshot(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	directory, err := s.MkdirAll(ctx, "/before/nested")
	require.NoError(t, err)
	file, err := s.CreateFile(ctx, directory.ID, "note.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = nodeByIDTx(tx, file.ID)
	require.NoError(t, err)
	_, _, err = s.Move(ctx, directory.ID, s.RootID(), "after", UnconditionalRev)
	require.NoError(t, err)
	got, err := nodeByPath(ctx, tx, s.RootID(), "/before/nested/note.txt")
	require.NoError(t, err)
	require.Equal(t, file, got)
	require.NoError(t, tx.Commit())
	_, err = s.NodeByPath(ctx, "/before/nested/note.txt")
	require.ErrorIs(t, err, ErrNotFound)
	got, err = s.NodeByPath(ctx, "/after/note.txt")
	require.NoError(t, err)
	require.Equal(t, file, got)
}

func TestPathsOfUsesCallerSnapshot(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	directory, err := s.MkdirAll(ctx, "/before/資料")
	require.NoError(t, err)
	file, err := s.CreateFile(ctx, directory.ID, "note.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = nodeByIDTx(tx, file.ID)
	require.NoError(t, err)

	_, _, err = s.Move(ctx, directory.ID, s.RootID(), "after", UnconditionalRev)
	require.NoError(t, err)
	ids := []int64{file.ID, s.RootID(), directory.ID, file.ID}
	paths, err := pathsOf(ctx, tx, ids)
	require.NoError(t, err)
	require.Equal(t, []string{
		"/before/資料/note.txt", "/", "/before/資料", "/before/資料/note.txt",
	}, paths)
	require.NoError(t, tx.Commit())

	paths, err = pathsOf(ctx, s.db, ids)
	require.NoError(t, err)
	require.Equal(t, []string{
		"/after/note.txt", "/", "/after", "/after/note.txt",
	}, paths)
}
