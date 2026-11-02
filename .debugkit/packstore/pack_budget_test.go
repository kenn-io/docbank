package packstore

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPackVerificationBudgetContinuesAndStillPacks(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	var entries []IndexEntry
	for _, value := range []byte{1, 2, 3} {
		content := bytes.Repeat([]byte{value}, 1024)
		entry := buildStoreTestPack(t, layout, content)
		catalog.members[entry.Hash] = Reference{Hash: entry.Hash}
		catalog.entries[entry.Hash] = entry
		catalog.packs[entry.PackID] = PackRecord{PackID: entry.PackID, EntryCount: 1, StoredBytes: entry.StoredLen, CreatedAt: time.Now()}
		entries = append(entries, entry)
	}
	slices.SortFunc(entries, func(a, b IndexEntry) int { return cmp.Compare(a.PackID, b.PackID) })
	// The second entry needs repair. A bounded first pass must leave it for
	// the next pass, without preventing new loose content from being packed.
	second := entries[1]
	content := bytes.Repeat([]byte{2}, 1024)
	for _, value := range []byte{1, 2, 3} {
		candidate := bytes.Repeat([]byte{value}, 1024)
		if hashForTest(candidate) == second.Hash {
			content = candidate
		}
	}
	writeMaintenanceLoose(t, layout, content)
	file, err := os.OpenFile(layout.PackPath(second.PackID), os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = file.WriteAt([]byte{0xff}, second.Offset)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	fresh := []byte("new content must not wait for a full inventory scan")
	freshHash := writeMaintenanceLoose(t, layout, fresh)
	catalog.addLoose(freshHash, layout.LoosePath(freshHash))
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	stats, err := maintainer.Pack(t.Context(), PackOptions{MaxVerifyBytes: 1})
	require.NoError(t, err)
	assert.Equal(t, 1, stats.BlobsChecked)
	assert.True(t, stats.VerificationPending)
	assert.Equal(t, 1, stats.BlobsPacked)
	assert.Equal(t, second.PackID, catalog.entries[second.Hash].PackID)
	got, _ := readStoreTest(t, maintainer.store, freshHash)
	assert.Equal(t, fresh, got)
	catalog.adoptErr = errors.New("catalog commit failed")
	_, err = maintainer.Pack(t.Context(), PackOptions{MaxVerifyBytes: 1})
	require.ErrorIs(t, err, catalog.adoptErr)
	catalog.adoptErr = nil
	stats, err = maintainer.Pack(t.Context(), PackOptions{MaxVerifyBytes: 1})
	require.NoError(t, err)
	assert.Equal(t, 1, stats.BlobsChecked)
	assert.Equal(t, 1, stats.BlobsPacked, "retry must revisit the repair, not just adopt its sealed orphan")
	assert.True(t, stats.VerificationPending)
	assert.NotEqual(t, second.PackID, catalog.entries[second.Hash].PackID)
	got, _ = readStoreTest(t, maintainer.store, second.Hash)
	assert.Equal(t, content, got)
	// Exhaustive callers still check the whole inventory, even mid-sweep.
	stats, err = maintainer.Pack(t.Context(), PackOptions{})
	require.NoError(t, err)
	assert.Equal(t, 4, stats.BlobsChecked)
	assert.False(t, stats.VerificationPending)
}

func TestPackVerificationSweepRestartsAndCancellationRetainsProgress(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	for _, body := range []string{"first packed entry", "second packed entry"} {
		entry := buildStoreTestPack(t, layout, []byte(body))
		catalog.members[entry.Hash] = Reference{Hash: entry.Hash}
		catalog.entries[entry.Hash] = entry
		catalog.packs[entry.PackID] = PackRecord{PackID: entry.PackID, EntryCount: 1, StoredBytes: entry.StoredLen, CreatedAt: time.Now()}
	}
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	for _, pending := range []bool{true, false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := maintainer.Pack(ctx, PackOptions{MaxVerifyBytes: 1})
		require.ErrorIs(t, err, context.Canceled)
		stats, err := maintainer.Pack(t.Context(), PackOptions{MaxVerifyBytes: 1})
		require.NoError(t, err)
		assert.Equal(t, 1, stats.BlobsChecked)
		assert.Equal(t, pending, stats.VerificationPending)
	}
}

func TestPackVerificationRevisitsRepairsDeferredByPackingBudget(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	var original []IndexEntry
	for _, value := range []byte{1, 2} {
		content := bytes.Repeat([]byte{value}, 1024)
		entry := buildStoreTestPack(t, layout, content)
		writeMaintenanceLoose(t, layout, content)
		catalog.members[entry.Hash] = Reference{Hash: entry.Hash}
		catalog.entries[entry.Hash] = entry
		catalog.packs[entry.PackID] = PackRecord{PackID: entry.PackID, EntryCount: 1, StoredBytes: entry.StoredLen, CreatedAt: time.Now()}
		original = append(original, entry)
		file, err := os.OpenFile(layout.PackPath(entry.PackID), os.O_RDWR, 0)
		require.NoError(t, err)
		_, err = file.WriteAt([]byte{0xff}, entry.Offset)
		require.NoError(t, err)
		require.NoError(t, file.Close())
	}
	healthy := buildStoreTestPack(t, layout, bytes.Repeat([]byte{3}, 1024))
	catalog.members[healthy.Hash] = Reference{Hash: healthy.Hash}
	catalog.entries[healthy.Hash] = healthy
	catalog.packs[healthy.PackID] = PackRecord{PackID: healthy.PackID, EntryCount: 1, StoredBytes: healthy.StoredLen, CreatedAt: time.Now()}
	require.Less(t, original[1].PackID, healthy.PackID)
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	for pass := range 2 {
		stats, err := maintainer.Pack(t.Context(), PackOptions{MaxBytes: 1, MaxVerifyBytes: 2048})
		require.NoError(t, err)
		if pass == 0 {
			assert.Equal(t, 2, stats.BlobsChecked)
			assert.True(t, stats.VerificationPending)
		}
		assert.Equal(t, 1, stats.BlobsPacked)
	}
	for _, entry := range original {
		assert.NotEqual(t, entry.PackID, catalog.entries[entry.Hash].PackID)
	}
}

func TestPackVerificationRevisitsEarlierEntriesDuringIngestion(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	content := []byte("original content that must be checked again")
	first := buildStoreTestPack(t, layout, content)
	second := buildStoreTestPack(t, layout, []byte("end of the initial inventory"))
	require.Less(t, first.PackID, second.PackID)
	for _, entry := range []IndexEntry{first, second} {
		catalog.members[entry.Hash] = Reference{Hash: entry.Hash}
		catalog.entries[entry.Hash] = entry
		catalog.packs[entry.PackID] = PackRecord{PackID: entry.PackID, EntryCount: 1, StoredBytes: entry.StoredLen, CreatedAt: time.Now()}
	}
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	for pass := range 3 {
		fresh := bytes.Repeat([]byte{byte(pass)}, 1024)
		hash := writeMaintenanceLoose(t, layout, fresh)
		catalog.addLoose(hash, layout.LoosePath(hash))
		stats, err := maintainer.Pack(t.Context(), PackOptions{MaxVerifyBytes: 1})
		require.NoError(t, err)
		assert.Equal(t, 1, stats.BlobsChecked)
		assert.Equal(t, pass != 1, stats.VerificationPending)
		assert.Contains(t, catalog.entries, hash, "new input must still be packed on every call")
		if pass == 0 {
			// Damage an entry already behind the cursor, with a valid recovery copy.
			writeMaintenanceLoose(t, layout, content)
			file, err := os.OpenFile(layout.PackPath(first.PackID), os.O_RDWR, 0)
			require.NoError(t, err)
			_, err = file.WriteAt([]byte{0xff}, first.Offset)
			require.NoError(t, err)
			require.NoError(t, file.Close())
		}
	}
	assert.NotEqual(t, first.PackID, catalog.entries[first.Hash].PackID)
	got, _ := readStoreTest(t, maintainer.store, first.Hash)
	assert.Equal(t, content, got)
}
