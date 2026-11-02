//go:build windows

package packstore

import (
	"bytes"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWindowsLooseRemovalUnlinksActiveStream(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	layout := layoutForStoreTest(t)
	loose, err := NewLooseStore(layout)
	require.NoError(err)
	content := bytes.Repeat([]byte("active Windows loose reader\n"), 128)
	written, err := loose.WriteBytes(t.Context(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})
	require.NoError(err)
	store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{
		written.Hash: {Member: true},
	}}, layout)
	stream, size, err := store.OpenStream(t.Context(), written.Hash)
	require.NoError(err)
	require.Equal(int64(len(content)), size)
	t.Cleanup(func() { require.NoError(stream.Close()) })
	prefix := make([]byte, 37)
	_, err = io.ReadFull(stream, prefix)
	require.NoError(err)

	err = loose.Remove(written.Hash, BestEffortRemoval)

	require.NoError(err)
	assert.NoFileExists(written.Path)
	assertNoLooseRemovalClaims(t, written.Path)
	remainder, err := io.ReadAll(stream)
	require.NoError(err)
	assert.Equal(content, slices.Concat(prefix, remainder))
	require.NoError(stream.Verify())
}

func TestWindowsLooseCleanupRemovesClaimDirectories(t *testing.T) {
	t.Run("explicit removal", func(t *testing.T) {
		store := newLooseStoreForTest(t, StagingSameDirectory)
		written, err := store.WriteBytes(t.Context(), []byte("Windows explicit loose removal"), WriteOptions{
			Durability: AtomicPublication,
			Dedup:      VerifyFullHash,
		})
		require.NoError(t, err)

		err = store.Remove(written.Hash, BestEffortRemoval)

		require.NoError(t, err)
		assert.NoFileExists(t, written.Path)
		assertNoLooseRemovalClaims(t, written.Path)
	})

	t.Run("redundant sweep", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		layout := layoutForStoreTest(t)
		content := []byte("Windows redundant loose sweep")
		entry := buildStoreTestPack(t, layout, content)
		require.Equal(entry.Hash, writeMaintenanceLoose(t, layout, content))
		catalog := newMaintenanceCatalog()
		catalog.members[entry.Hash] = Reference{Hash: entry.Hash}
		catalog.entries[entry.Hash] = entry
		catalog.packs[entry.PackID] = PackRecord{
			PackID: entry.PackID, EntryCount: 1, StoredBytes: entry.StoredLen, CreatedAt: time.Now(),
		}
		maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

		stats, err := maintainer.Pack(t.Context(), PackOptions{})

		require.NoError(err)
		assert.Equal(1, stats.LooseSwept)
		assert.NoFileExists(layout.LoosePath(entry.Hash))
		assertNoLooseRemovalClaims(t, layout.LoosePath(entry.Hash))
	})

	t.Run("packed source", func(t *testing.T) {
		layout := layoutForStoreTest(t)
		content := []byte("Windows packed loose source")
		hash := writeMaintenanceLoose(t, layout, content)
		catalog := newMaintenanceCatalog()
		catalog.addLoose(hash, layout.LoosePath(hash))
		maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

		stats, err := maintainer.Pack(t.Context(), PackOptions{})

		require.NoError(t, err)
		assert.Equal(t, 1, stats.BlobsPacked)
		assert.NoFileExists(t, layout.LoosePath(hash))
		assertNoLooseRemovalClaims(t, layout.LoosePath(hash))
	})
}
