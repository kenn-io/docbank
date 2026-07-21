package docbank

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWalkKeepsStableSnapshotAcrossConcurrentMutations(t *testing.T) {
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })

	alpha, err := vault.Put(t.Context(), "/alpha.txt", strings.NewReader("alpha\n"), PutOptions{})
	require.NoError(t, err)
	bravo, err := vault.Put(t.Context(), "/bravo.txt", strings.NewReader("bravo\n"), PutOptions{})
	require.NoError(t, err)
	charlie, err := vault.Put(t.Context(), "/charlie.txt", strings.NewReader("charlie\n"), PutOptions{})
	require.NoError(t, err)
	delta, err := vault.Put(t.Context(), "/folder/delta.txt", strings.NewReader("delta\n"), PutOptions{})
	require.NoError(t, err)
	root, err := vault.Stat(t.Context(), "/")
	require.NoError(t, err)
	folder, err := vault.Stat(t.Context(), "/folder")
	require.NoError(t, err)

	walker, err := vault.Walk(t.Context(), "/", WalkOptions{PageSize: 2})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, walker.Close()) })

	first, err := walker.Next(t.Context())
	require.NoError(t, err)
	require.Equal(t, []WalkEntry{
		{Path: "/", Node: root},
		{Path: "/alpha.txt", Node: alpha.Node},
	}, first)

	mutationErr := make(chan error, 1)
	go func() {
		if _, err := vault.Put(context.Background(), "/added.txt", strings.NewReader("added\n"), PutOptions{}); err != nil {
			mutationErr <- err
			return
		}
		if _, err := vault.MovePath(context.Background(), "/bravo.txt", "/zulu.txt", RevisionOptions{}); err != nil {
			mutationErr <- err
			return
		}
		_, err := vault.TrashPath(context.Background(), "/folder", RevisionOptions{})
		mutationErr <- err
	}()
	require.NoError(t, <-mutationErr)

	entries := append([]WalkEntry(nil), first...)
	for {
		page, err := walker.Next(t.Context())
		if err != nil {
			require.ErrorIs(t, err, io.EOF)
			break
		}
		require.LessOrEqual(t, len(page), 2)
		entries = append(entries, page...)
	}

	require.Equal(t, []WalkEntry{
		{Path: "/", Node: root},
		{Path: "/alpha.txt", Node: alpha.Node},
		{Path: "/bravo.txt", Node: bravo.Node},
		{Path: "/charlie.txt", Node: charlie.Node},
		{Path: "/folder", Node: folder},
		{Path: "/folder/delta.txt", Node: delta.Node},
	}, entries)

	seen := make(map[int64]struct{}, len(entries))
	for _, entry := range entries {
		_, duplicate := seen[entry.Node.ID]
		assert.False(t, duplicate, "node %d appeared more than once", entry.Node.ID)
		seen[entry.Node.ID] = struct{}{}
	}
	assert.Len(t, seen, 6)
}

func TestWalkPinsSubtreeSnapshotBeforeFirstPage(t *testing.T) {
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	original, err := vault.Put(t.Context(), "/scope/original.txt", strings.NewReader("original\n"), PutOptions{})
	require.NoError(t, err)
	_, err = vault.Put(t.Context(), "/outside.txt", strings.NewReader("outside\n"), PutOptions{})
	require.NoError(t, err)
	scope, err := vault.Stat(t.Context(), "/scope")
	require.NoError(t, err)

	walker, err := vault.Walk(t.Context(), "/scope", WalkOptions{PageSize: 1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, walker.Close()) })
	_, err = vault.MovePath(t.Context(), "/scope/original.txt", "/moved.txt", RevisionOptions{})
	require.NoError(t, err)
	_, err = vault.Put(t.Context(), "/scope/later.txt", strings.NewReader("later\n"), PutOptions{})
	require.NoError(t, err)

	var entries []WalkEntry
	for {
		page, err := walker.Next(t.Context())
		if err != nil {
			require.ErrorIs(t, err, io.EOF)
			break
		}
		entries = append(entries, page...)
	}
	assert.Equal(t, []WalkEntry{
		{Path: "/scope", Node: scope},
		{Path: "/scope/original.txt", Node: original.Node},
	}, entries)
}

func TestWalkNextHonorsCancellation(t *testing.T) {
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	_, err = vault.Put(t.Context(), "/item.txt", strings.NewReader("item\n"), PutOptions{})
	require.NoError(t, err)

	walker, err := vault.Walk(t.Context(), "/", WalkOptions{PageSize: 1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, walker.Close()) })
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	page, err := walker.Next(canceled)
	assert.Nil(t, page)
	require.ErrorIs(t, err, context.Canceled)
	page, err = walker.Next(t.Context())
	require.NoError(t, err)
	require.Len(t, page, 1)
	assert.Equal(t, "/", page[0].Path)
}

func TestWalkOptionallyIncludesTrashedNodes(t *testing.T) {
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	created, err := vault.Put(t.Context(), "/trashed.txt", strings.NewReader("trashed\n"), PutOptions{})
	require.NoError(t, err)
	trashed, err := vault.TrashPath(t.Context(), "/trashed.txt", RevisionOptions{
		IfRevision: created.Node.Revision,
	})
	require.NoError(t, err)

	walker, err := vault.Walk(t.Context(), "/", WalkOptions{PageSize: 2, IncludeTrashed: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, walker.Close()) })
	page, err := walker.Next(t.Context())
	require.NoError(t, err)
	require.Len(t, page, 2)
	assert.Equal(t, "/trashed.txt", page[1].Path)
	assert.Equal(t, trashed.Node, page[1].Node)
}

func TestWalkUsesFiniteDefaultAndRejectsInvalidPageSizes(t *testing.T) {
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	for i := range DefaultWalkPageSize + 1 {
		_, err := vault.metadata.Mkdir(t.Context(), vault.metadata.RootID(),
			fmt.Sprintf("node-%04d", i))
		require.NoError(t, err)
	}

	walker, err := vault.Walk(t.Context(), "/", WalkOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, walker.Close()) })
	first, err := walker.Next(t.Context())
	require.NoError(t, err)
	assert.Len(t, first, DefaultWalkPageSize)
	second, err := walker.Next(t.Context())
	require.NoError(t, err)
	assert.Len(t, second, 2)

	for _, pageSize := range []int{-1, MaxWalkPageSize + 1} {
		invalid, err := vault.Walk(t.Context(), "/", WalkOptions{PageSize: pageSize})
		assert.Nil(t, invalid)
		require.ErrorContains(t, err, "page size must be between 1 and 5000")
	}
}

func TestWalkAcceptsMaximumPageSize(t *testing.T) {
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })

	walker, err := vault.Walk(t.Context(), "/", WalkOptions{PageSize: MaxWalkPageSize})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, walker.Close()) })
	page, err := walker.Next(t.Context())
	require.NoError(t, err)
	require.Len(t, page, 1)
	assert.Equal(t, "/", page[0].Path)
}

func TestWalkCloseIsIdempotentAndReleasesVaultLifecycle(t *testing.T) {
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	walker, err := vault.Walk(t.Context(), "/", WalkOptions{PageSize: 1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, walker.Close()) })

	closed := make(chan struct{})
	var closeErr error
	go func() {
		closeErr = vault.Close()
		close(closed)
	}()
	require.Eventually(t, func() bool {
		if vault.lifecycle.TryRLock() {
			vault.lifecycle.RUnlock()
			return false
		}
		return true
	}, time.Second, time.Millisecond, "Vault.Close never queued for the lifecycle write lock")
	select {
	case <-closed:
		require.Fail(t, "Vault.Close returned while the walker still held its lifecycle lease")
	default:
	}

	require.NoError(t, walker.Close())
	require.NoError(t, walker.Close())
	select {
	case <-closed:
	case <-time.After(time.Second):
		require.Fail(t, "Vault.Close did not return after the walker released its lifecycle lease")
	}
	require.NoError(t, closeErr)
	page, err := walker.Next(t.Context())
	assert.Nil(t, page)
	require.ErrorIs(t, err, io.EOF)
}

func TestWalkCloseReleasesReadTransaction(t *testing.T) {
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	_, err = vault.Put(t.Context(), "/before.txt", strings.NewReader("before\n"), PutOptions{})
	require.NoError(t, err)

	walker, err := vault.Walk(t.Context(), "/", WalkOptions{PageSize: 1})
	require.NoError(t, err)
	_, err = vault.Put(t.Context(), "/after.txt", strings.NewReader("after\n"), PutOptions{})
	require.NoError(t, err)
	require.NoError(t, walker.Close())
	require.NoError(t, vault.metadata.Checkpoint(t.Context()))
}

func TestWalkerConcurrentNextAndClose(t *testing.T) {
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })

	type nextResult struct {
		page []WalkEntry
		err  error
	}
	for range 100 {
		walker, err := vault.Walk(t.Context(), "/", WalkOptions{PageSize: 1})
		require.NoError(t, err)
		start := make(chan struct{})
		nextDone := make(chan nextResult, 1)
		closeDone := make(chan error, 1)
		go func() {
			<-start
			page, err := walker.Next(t.Context())
			nextDone <- nextResult{page: page, err: err}
		}()
		go func() {
			<-start
			closeDone <- walker.Close()
		}()
		close(start)

		result := <-nextDone
		if result.err != nil {
			assert.ErrorIs(t, result.err, io.EOF)
			assert.Nil(t, result.page)
		} else {
			require.Len(t, result.page, 1)
			assert.Equal(t, "/", result.page[0].Path)
		}
		require.NoError(t, <-closeDone)
	}
}
