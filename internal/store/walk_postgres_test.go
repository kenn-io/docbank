package store

import (
	"context"
	"crypto/sha256"
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

func checkWalkLongPathsAndByteOrderPostgres(t *testing.T) {
	t.Helper()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	a, err := s.Mkdir(ctx, s.RootID(), "a")
	require.NoError(t, err)
	_, err = s.Mkdir(ctx, s.RootID(), "a.z")
	require.NoError(t, err)
	parent, err := s.Mkdir(ctx, a.ID, "b")
	require.NoError(t, err)
	want := []string{"/", "/a", "/a.z", "/a/b"}
	path := "/a/b"
	for depth := range 28 {
		var label strings.Builder
		label.WriteString("李")
		for block := range 8 {
			hash := sha256.Sum256([]byte(fmt.Sprintf("%d/%d", depth, block)))
			fmt.Fprintf(&label, "%x", hash)
		}
		name := label.String()
		parent, err = s.Mkdir(ctx, parent.ID, name)
		require.NoError(t, err)
		path += "/" + name
		want = append(want, path)
	}
	require.Greater(t, len(path), 14<<10)
	require.Less(t, len(path)+2, MaxWalkPathBytes)
	for _, name := range []string{"b", "a"} {
		_, err := s.Mkdir(ctx, parent.ID, name)
		require.NoError(t, err)
	}
	want = append(want, path+"/a", path+"/b")
	walk, err := s.BeginWalk(ctx, "/", 5, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, walk.Close()) })
	var got []string
	for _, entry := range collectStoreWalk(t, walk) {
		got = append(got, entry.Path)
	}
	require.Equal(t, want, got)
}

func checkWalkActiveQueryCancellationPostgres(t *testing.T) {
	t.Helper()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	for _, name := range []string{"a", "b"} {
		_, err := s.Mkdir(ctx, s.RootID(), name)
		require.NoError(t, err)
	}
	walk, err := s.BeginWalk(ctx, "/", 3, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, walk.Close()) })
	var pid int
	require.NoError(t, walk.tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
	holder, err := s.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = holder.Rollback() }()
	const key = int64(912081)
	_, err = holder.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, key)
	require.NoError(t, err)
	// Block a real SQL command after the page has already changed its frontier.
	walk.pageContextErr = func(ctx context.Context, examined int64) error {
		if examined == 1 {
			_, err := walk.tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, key)
			return err
		}
		return ctx.Err()
	}
	pageCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	before := walk.Stats()
	go func() { _, err := walk.Next(pageCtx); done <- err }()
	// Observe the blocked command through the server, without timed cancellation.
	for waiting := false; !waiting; {
		select {
		case err := <-done:
			t.Fatalf("page returned before cancellation: %v", err)
		default:
		}
		require.NoError(t, s.db.QueryRowContext(ctx, `SELECT EXISTS (
   SELECT 1 FROM pg_catalog.pg_locks WHERE pid=$1 AND locktype='advisory' AND NOT granted
  )`, pid).Scan(&waiting))
	}
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.Equal(t, before, walk.Stats())
	require.NoError(t, holder.Rollback())
	walk.pageContextErr = func(ctx context.Context, _ int64) error { return ctx.Err() }
	_, err = s.Mkdir(ctx, s.RootID(), "after-cancel")
	require.NoError(t, err)
	var got []string
	for _, entry := range collectStoreWalk(t, walk) {
		got = append(got, entry.Path)
	}
	require.Equal(t, []string{"/", "/a", "/b"}, got)
}
