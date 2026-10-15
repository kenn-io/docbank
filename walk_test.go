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

func TestWalkKeepsStableSnapshotAcrossMutations(t *testing.T) {
	t.Run("subtree mutations before the first page", func(t *testing.T) {
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
	})

	t.Run("root mutations between pages", func(t *testing.T) {
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

		_, err = vault.Put(t.Context(), "/added.txt", strings.NewReader("added\n"), PutOptions{})
		require.NoError(t, err)
		_, err = vault.MovePath(t.Context(), "/bravo.txt", "/zulu.txt", RevisionOptions{})
		require.NoError(t, err)
		_, err = vault.TrashPath(t.Context(), "/folder", RevisionOptions{})
		require.NoError(t, err)

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
	})
}

func TestWalkOutlivesCanceledSetupContext(t *testing.T) {
	for _, test := range maintenanceDrivers() {
		t.Run(test.name, func(t *testing.T) {
			vault := newMaintenanceVault(t, test.driver)
			_, err := vault.Put(t.Context(), "/item.txt", strings.NewReader("item\n"), PutOptions{})
			require.NoError(t, err)

			setupCtx, cancel := context.WithCancel(t.Context())
			walker, err := vault.Walk(setupCtx, "/", WalkOptions{PageSize: 1})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, walker.Close()) })
			cancel()

			page, err := walker.Next(t.Context())
			require.NoError(t, err)
			require.Len(t, page, 1)
			assert.Equal(t, "/", page[0].Path)
		})
	}
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

func TestWalkPageSizeBounds(t *testing.T) {
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	for i := range DefaultWalkPageSize + 1 {
		_, err := vault.metadata.Mkdir(t.Context(), vault.metadata.RootID(),
			fmt.Sprintf("node-%04d", i))
		require.NoError(t, err)
	}

	t.Run("default", func(t *testing.T) {
		walker, err := vault.Walk(t.Context(), "/", WalkOptions{})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, walker.Close()) })
		first, err := walker.Next(t.Context())
		require.NoError(t, err)
		assert.Len(t, first, DefaultWalkPageSize)
		second, err := walker.Next(t.Context())
		require.NoError(t, err)
		assert.Len(t, second, 2)
	})
	t.Run("invalid", func(t *testing.T) {
		for _, pageSize := range []int{-1, MaxWalkPageSize + 1} {
			invalid, err := vault.Walk(t.Context(), "/", WalkOptions{PageSize: pageSize})
			assert.Nil(t, invalid)
			require.ErrorContains(t, err, "page size must be between 1 and 5000")
		}
	})
	t.Run("maximum", func(t *testing.T) {
		walker, err := vault.Walk(t.Context(), "/", WalkOptions{PageSize: MaxWalkPageSize})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, walker.Close()) })
		page, err := walker.Next(t.Context())
		require.NoError(t, err)
		require.Len(t, page, DefaultWalkPageSize+2)
		assert.Equal(t, "/", page[0].Path)
	})
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
	}, 30*time.Second, time.Millisecond, "Vault.Close never queued for the lifecycle write lock")
	select {
	case <-closed:
		require.Fail(t, "Vault.Close returned while the walker still held its lifecycle lease")
	default:
	}

	require.NoError(t, walker.Close())
	require.NoError(t, walker.Close())
	<-closed
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

func TestWalkerZeroValueCloseIsSafe(t *testing.T) {
	walker := new(Walker)
	require.NoError(t, walker.Close())
	require.NoError(t, walker.Close())
}
