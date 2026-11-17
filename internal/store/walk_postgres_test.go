package store

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func checkWalkSnapshotPostgres(t *testing.T) {
	t.Helper()
	s := newPostgresQueryStore(t)
	s.db.SetMaxOpenConns(1)
	ctx := t.Context()
	var renamed Node
	for _, name := range []string{"B", "Z", "_x", "a", "b", "café", "李"} {
		n, err := s.Mkdir(ctx, s.RootID(), name)
		require.NoError(t, err)
		if name == "Z" {
			renamed = n
		}
	}
	walk, err := s.BeginWalk(ctx, "/", 2, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, walk.Close()) })
	first, err := walk.Next(ctx)
	require.NoError(t, err)
	require.Len(t, first, 2)
	_, _, err = s.Move(ctx, renamed.ID, s.RootID(), "replacement", renamed.Revision)
	require.NoError(t, err)
	_, err = s.Mkdir(ctx, s.RootID(), "new")
	require.NoError(t, err)
	first = append(first, collectStoreWalk(t, walk)...)
	var paths []string
	for _, entry := range first {
		paths = append(paths, entry.Path)
		if entry.Node.ID == renamed.ID {
			require.Equal(t, "Z", entry.Node.Name)
		}
	}
	require.Equal(t, []string{"/", "/B", "/Z", "/_x", "/a", "/b", "/café", "/李"}, paths)
	require.NoError(t, walk.Close())
	fresh, err := s.BeginWalk(ctx, "/", 2, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, fresh.Close()) })
	paths = nil
	for _, entry := range collectStoreWalk(t, fresh) {
		paths = append(paths, entry.Path)
	}
	require.Contains(t, paths, "/replacement")
	require.Contains(t, paths, "/new")
	require.NotContains(t, paths, "/Z")
	require.NoError(t, fresh.Close())
	_, err = s.Mkdir(ctx, s.RootID(), "after-close")
	require.NoError(t, err, "closing a walk must not close the Store pool")
}

func checkWalkSeeksPostgres(t *testing.T) {
	t.Helper()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	for i := range 1000 {
		_, err := s.Mkdir(ctx, s.RootID(), fmt.Sprintf("node-%04d", i))
		require.NoError(t, err)
	}
	for _, trash := range []bool{false, true} {
		statement, args := walkSeekQuery(s.RootID(), trash, "node-0900", 0, false, true)
		rows, err := s.db.QueryContext(ctx, "EXPLAIN "+statement, args...)
		require.NoError(t, err)
		defer func() { require.NoError(t, rows.Close()) }()
		var plan []string
		for rows.Next() {
			var line string
			require.NoError(t, rows.Scan(&line))
			plan = append(plan, line)
		}
		require.NoError(t, rows.Err())
		require.Regexp(t, `Index (Only )?Scan`, strings.Join(plan, "\n"))
		require.Contains(t, strings.Join(plan, "\n"), "Index Cond:")
		require.Contains(t, strings.Join(plan, "\n"), "parent_id =")
		require.Contains(t, strings.Join(plan, "\n"), "name ")
		require.NotContains(t, strings.Join(plan, "\n"), "Sort")
	}
}
