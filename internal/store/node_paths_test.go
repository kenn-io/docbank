package store

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

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
