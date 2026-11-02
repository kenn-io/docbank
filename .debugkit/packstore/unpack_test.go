package packstore

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnpackDurablyRestoresAllLiveContentThenClearsMetadata(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	for _, content := range [][]byte{nil, []byte("packed content")} {
		entry := buildStoreTestPack(t, layout, content)
		catalog.members[entry.Hash] = Reference{Hash: entry.Hash, OriginalHashes: []string{entry.Hash.String()}}
		catalog.entries[entry.Hash] = entry
		catalog.packs[entry.PackID] = PackRecord{
			PackID: entry.PackID, EntryCount: 1,
			StoredBytes: entry.StoredLen, CreatedAt: time.Now().UTC(),
		}
	}
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Unpack(t.Context())
	require.NoError(err)
	assert.Equal(2, stats.PacksUnpacked)
	assert.Equal(2, stats.BlobsRestored)
	entries, packs := catalog.snapshot()
	assert.Empty(entries)
	assert.Empty(packs)
	for hash := range catalog.members {
		assert.FileExists(layout.LoosePath(hash))
		data, readErr := os.ReadFile(layout.LoosePath(hash))
		require.NoError(readErr)
		assert.Equal(hash, hashForTest(data))
	}
}

func TestUnpackPreflightsEveryPackBeforeWritingLooseContent(t *testing.T) {
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	first := buildStoreTestPack(t, layout, []byte("first"))
	second := buildStoreTestPack(t, layout, []byte("second"))
	for _, entry := range []IndexEntry{first, second} {
		catalog.members[entry.Hash] = Reference{Hash: entry.Hash}
		catalog.entries[entry.Hash] = entry
		catalog.packs[entry.PackID] = PackRecord{
			PackID: entry.PackID, EntryCount: 1,
			StoredBytes: entry.StoredLen, CreatedAt: time.Now().UTC(),
		}
	}
	second.RawLen++
	catalog.entries[second.Hash] = second
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	_, err := maintainer.Unpack(t.Context())
	require.ErrorContains(t, err, "metadata mismatch")
	assert.NoFileExists(layout.LoosePath(first.Hash))
	assert.NoFileExists(layout.LoosePath(second.Hash))
	entries, _ := catalog.snapshot()
	assert.Len(entries, 2)
}

func TestUnpackRejectsOversizedLiveEntryBeforeWrites(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	entry := buildStoreTestPack(t, layout, []byte("ninebytes"))
	catalog.members[entry.Hash] = Reference{Hash: entry.Hash}
	catalog.entries[entry.Hash] = entry
	catalog.packs[entry.PackID] = PackRecord{
		PackID: entry.PackID, EntryCount: 1,
		StoredBytes: entry.StoredLen, CreatedAt: time.Now().UTC(),
	}
	limits := DefaultLimits()
	limits.BlobBytes = 8
	maintainer := newMaintainerForTest(t, catalog, layout, limits)

	_, err := maintainer.Unpack(t.Context())
	require.ErrorIs(t, err, ErrBlobTooLarge)
	assert.NoFileExists(t, layout.LoosePath(entry.Hash))
}

func TestUnpackHonorsCancellationBeforeMutation(t *testing.T) {
	maintainer := newMaintainerForTest(t, newMaintenanceCatalog(), layoutForStoreTest(t), DefaultLimits())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := maintainer.Unpack(ctx)
	require.ErrorIs(t, err, context.Canceled)
}
