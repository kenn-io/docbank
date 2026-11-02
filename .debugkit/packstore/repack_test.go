package packstore

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestRepackRetiresZeroLiveBeforeRewritingSparsePack(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	zero := buildStoreTestPack(t, layout, []byte("dead"))
	catalog.packs[zero.PackID] = PackRecord{
		PackID: zero.PackID, EntryCount: 1,
		StoredBytes: zero.StoredLen, CreatedAt: time.Now().Add(-48 * time.Hour),
	}
	live, deadA, deadB := buildSparsePack(t, layout)
	catalog.members[live.Hash] = Reference{Hash: live.Hash}
	catalog.entries[live.Hash] = live
	catalog.entries[deadA.Hash] = deadA
	catalog.entries[deadB.Hash] = deadB
	catalog.packs[live.PackID] = PackRecord{
		PackID: live.PackID, EntryCount: 3,
		StoredBytes: live.StoredLen + deadA.StoredLen + deadB.StoredLen, CreatedAt: time.Now().Add(-48 * time.Hour),
	}
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Repack(t.Context(), RepackOptions{
		Now: time.Now(), MaxBytes: 1, Selection: RepackSelection{MinAge: time.Nanosecond, MinDeadStored: 1},
	})
	require.NoError(err)
	assert.Equal(2, stats.PacksSelected)
	assert.Equal(1, stats.PacksRewritten)
	assert.Equal(2, stats.PacksRemoved)
	assert.Equal(1, stats.BlobsRepacked)
	assert.NoFileExists(layout.PackPath(zero.PackID))
	assert.NoFileExists(layout.PackPath(live.PackID))
	got, _ := readStoreTest(t, maintainer.store, live.Hash)
	assert.Equal([]byte("live sparse content"), got)
}

func TestRepackDefersPackWithOversizedLiveEntry(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	live, deadA, deadB := buildSparsePack(t, layout)
	catalog.members[live.Hash] = Reference{Hash: live.Hash}
	live.RawLen = 9
	catalog.entries[live.Hash] = live
	catalog.entries[deadA.Hash] = deadA
	catalog.entries[deadB.Hash] = deadB
	catalog.packs[live.PackID] = PackRecord{
		PackID: live.PackID, EntryCount: 3,
		StoredBytes: live.StoredLen + deadA.StoredLen + deadB.StoredLen, CreatedAt: time.Now().Add(-48 * time.Hour),
	}
	limits := DefaultLimits()
	limits.BlobBytes = 8
	maintainer := newMaintainerForTest(t, catalog, layout, limits)

	stats, err := maintainer.Repack(t.Context(), RepackOptions{
		Selection: RepackSelection{MinAge: time.Nanosecond, MinDeadStored: 1},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, stats.PacksDeferredOversized)
	assert.FileExists(t, layout.PackPath(live.PackID))
}

func TestAutomaticRepackContinuesPastCorruptSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	badLive, badDeadA, badDeadB := buildSparsePack(t, layout)
	goodLive, goodDeadA, goodDeadB := buildSparsePackWithContent(t, layout, "good live", "good dead")
	for _, group := range [][3]IndexEntry{{badLive, badDeadA, badDeadB}, {goodLive, goodDeadA, goodDeadB}} {
		catalog.members[group[0].Hash] = Reference{Hash: group[0].Hash}
		for _, entry := range group {
			catalog.entries[entry.Hash] = entry
		}
		catalog.packs[group[0].PackID] = PackRecord{
			PackID: group[0].PackID, EntryCount: 3,
			StoredBytes: group[0].StoredLen + group[1].StoredLen + group[2].StoredLen, CreatedAt: time.Now().Add(-48 * time.Hour),
		}
	}
	require.NoError(os.WriteFile(layout.PackPath(badLive.PackID), []byte("truncated"), 0o600))
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Repack(t.Context(), RepackOptions{
		MaxBytes: 1 << 20, Selection: RepackSelection{MinAge: time.Nanosecond, MinDeadStored: 1},
	})
	require.Error(err)
	require.ErrorContains(err, badLive.PackID, "aggregate errors identify the corrupt source pack")
	assert.Equal(1, stats.PacksRewritten)
	assert.FileExists(layout.PackPath(badLive.PackID))
	assert.NoFileExists(layout.PackPath(goodLive.PackID))
	got, _ := readStoreTest(t, maintainer.store, goodLive.Hash)
	assert.Equal([]byte("good live"), got)
}

func TestRepackCatalogCASFailureKeepsSourceAuthority(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	live, deadA, deadB := buildSparsePack(t, layout)
	catalog.members[live.Hash] = Reference{Hash: live.Hash}
	catalog.entries[live.Hash] = live
	catalog.entries[deadA.Hash] = deadA
	catalog.entries[deadB.Hash] = deadB
	catalog.packs[live.PackID] = PackRecord{
		PackID: live.PackID, EntryCount: 3,
		StoredBytes: live.StoredLen + deadA.StoredLen + deadB.StoredLen, CreatedAt: time.Now().Add(-48 * time.Hour),
	}
	catalog.repackErr = errors.New("injected exact-set failure")
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	_, err := maintainer.Repack(t.Context(), RepackOptions{
		Selection: RepackSelection{MinAge: time.Nanosecond, MinDeadStored: 1},
	})
	require.ErrorContains(t, err, "exact-set failure")
	entries, _ := catalog.snapshot()
	assert.Equal(t, live.PackID, entries[live.Hash].PackID)
	assert.FileExists(t, layout.PackPath(live.PackID))
}

func TestRewriteSourceRotatesBeforeExceedingOutputEntryLimit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	first, second, third := buildSparsePack(t, layout)
	entries := []IndexEntry{first, second, third}
	for _, entry := range entries {
		catalog.members[entry.Hash] = Reference{Hash: entry.Hash}
		catalog.entries[entry.Hash] = entry
	}
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	maintainer.limits.PackEntries = 1

	result, err := maintainer.rewriteSource(t.Context(), first.PackID, entries, 0)
	require.NoError(err)
	require.Len(result.records, 3)
	for _, record := range result.records {
		assert.Equal(int64(1), record.EntryCount)
		reader, openErr := OpenMaintenancePack(layout.PackPath(record.PackID), maintainer.limits)
		require.NoError(openErr)
		require.NoError(reader.Close())
	}
}

func TestRetireEmptyKeepsPackWhenCatalogStillHasMappings(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	entry := buildStoreTestPack(t, layout, []byte("still live"))
	catalog.members[entry.Hash] = Reference{Hash: entry.Hash}
	catalog.entries[entry.Hash] = entry
	catalog.packs[entry.PackID] = PackRecord{
		PackID: entry.PackID, EntryCount: 1,
		StoredBytes: entry.StoredLen, CreatedAt: time.Now().UTC(),
	}
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	err := maintainer.retireEmpty(t.Context(), entry.PackID, &RepackStats{})
	require.ErrorContains(t, err, "still has mappings")
	assert.FileExists(t, layout.PackPath(entry.PackID))
}

func buildSparsePack(t *testing.T, layout Layout) (IndexEntry, IndexEntry, IndexEntry) {
	t.Helper()
	return buildSparsePackWithContent(t, layout, "live sparse content", "dead sparse content")
}

func buildSparsePackWithContent(t *testing.T, layout Layout, liveContent, deadContent string) (IndexEntry, IndexEntry, IndexEntry) {
	t.Helper()
	require := require.New(t)
	t.Helper()
	writer, err := pack.NewWriter(t.TempDir(), pack.WriterOptions{})
	require.NoError(err)
	_, err = writer.Append([]byte(liveContent))
	require.NoError(err)
	_, err = writer.Append([]byte(deadContent + "-one"))
	require.NoError(err)
	_, err = writer.Append([]byte(deadContent + "-two"))
	require.NoError(err)
	packID := writer.ID()
	entries, err := writer.Seal(layout.PackPath(packID))
	require.NoError(err)
	require.Len(entries, 3)
	return indexFromPack(packID, entries[0]), indexFromPack(packID, entries[1]), indexFromPack(packID, entries[2])
}
