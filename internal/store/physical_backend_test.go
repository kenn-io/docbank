package store

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
	"go.kenn.io/kit/packstore/packstoretest"
)

func checkPackCatalogContractPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	packstoretest.RunCatalogContract(t, func(t *testing.T) packstoretest.CatalogHarness {
		t.Helper()
		return &docbankPackHarness{t: t, store: newPostgresQueryStore(t)}
	}, packstoretest.ContractOptions{Now: time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC), NewPackID: pack.NewPackID})
}

func checkPhysicalStorageLifecyclePostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	catalog := NewPackCatalog(s)
	layout, err := packstore.NewLayout(t.TempDir(), packstore.LayoutOptions{Staging: packstore.StagingSameDirectory})
	require.NoError(t, err)
	maintainer, err := packstore.NewMaintainer(catalog, layout, packstore.MaintainerOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, maintainer.Close()) })
	var nodes []Node
	var hashes []packstore.Hash
	contents := []string{"first retained document", "second retained document", "surviving document"}
	for i, content := range contents {
		hash, err := packstore.ParseHash(fmt.Sprintf("%x", sha256.Sum256([]byte(content))))
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Dir(layout.LoosePath(hash)), 0o700))
		require.NoError(t, os.WriteFile(layout.LoosePath(hash), []byte(content), 0o600))
		node, err := s.CreateFile(ctx, s.RootID(), fmt.Sprintf("document-%d.txt", i), hash.String(), int64(len(content)), "text/plain")
		require.NoError(t, err)
		nodes = append(nodes, node)
		hashes = append(hashes, hash)
	}
	packed, err := maintainer.Pack(ctx, packstore.PackOptions{})
	require.NoError(t, err)
	require.Equal(t, 3, packed.BlobsPacked)
	require.Equal(t, 1, packed.PacksSealed)
	usage, err := catalog.ListPackUsage(ctx)
	require.NoError(t, err)
	require.Len(t, usage, 1)
	require.EqualValues(t, 3, usage[0].LiveEntries)
	for i, hash := range hashes {
		got, _, err := maintainer.Store().ReadBounded(ctx, hash, 1024)
		require.NoError(t, err)
		require.Equal(t, contents[i], string(got))
		require.NoFileExists(t, layout.LoosePath(hash))
	}
	for _, node := range nodes[:2] {
		_, _, err := s.Trash(ctx, node.ID, -1)
		require.NoError(t, err)
	}
	_, err = s.TrashEmpty(ctx, 0, true)
	require.NoError(t, err)
	pending := GCPackRetirement{StoreID: string(catalog.PrimaryStoreID()), PackID: usage[0].PackID}
	require.NoError(t, s.DeleteBlobRowsWithGCRetirements(ctx, []string{hashes[0].String(), hashes[1].String()}, nil, []GCPackRetirement{pending}))
	usage, err = catalog.ListPackUsage(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, usage[0].LiveEntries)
	require.EqualValues(t, len(contents[2]), usage[0].LiveRawBytes)
	require.EqualValues(t, len(contents[2]), usage[0].MaxLiveRawLen)
	for _, hash := range hashes[:2] {
		_, _, err := maintainer.Store().ReadBounded(ctx, hash, 1024)
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	retirements, err := s.PendingDerivativePackRetirements(ctx, 1, nil)
	require.NoError(t, err)
	require.Equal(t, usage, retirements)
	empty, err := s.PendingDerivativePackRetirements(ctx, 1, []string{})
	require.NoError(t, err)
	require.Empty(t, empty)
	removed, err := s.PruneDerivativePackRetirementMappings(ctx, []string{pending.PackID})
	require.NoError(t, err)
	require.EqualValues(t, 2, removed)
	repacked, err := maintainer.Repack(ctx, packstore.RepackOptions{
		Now:       time.Now().UTC().Add(48 * time.Hour),
		Selection: packstore.RepackSelection{MinAge: time.Nanosecond, MinDeadStored: 1},
	})
	require.NoError(t, err)
	require.Equal(t, 1, repacked.PacksRewritten)
	require.Equal(t, 1, repacked.PacksRemoved)
	got, _, err := maintainer.Store().ReadBounded(ctx, hashes[2], 1024)
	require.NoError(t, err)
	require.Equal(t, contents[2], string(got))
	retirements, err = s.PendingDerivativePackRetirements(ctx, 1, nil)
	require.NoError(t, err)
	require.Empty(t, retirements)
	checkRetentionMetadata(t, s)
}

func checkPhysicalCleanupRecoveryPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	orphan, err := packstore.ParseHash(fakeHash("aa"))
	require.NoError(t, err)
	liveHash := fakeHash("bb")
	_, err = s.CreateFile(ctx, s.RootID(), "live.txt", liveHash, 13, "text/plain")
	require.NoError(t, err)
	h := &docbankPackHarness{t: t, store: s}
	h.SetMember(orphan, true)
	receipt := GCLooseRetirement{StoreID: s.primaryStoreID, Hash: orphan, Encoding: packstore.LooseEncodingRaw}
	// The first deletion succeeds, the retained original fails: membership and
	// the cleanup receipt must roll back together.
	require.Error(t, s.DeleteBlobRowsWithGCRetirements(ctx, []string{orphan.String(), liveHash}, []GCLooseRetirement{receipt}, nil))
	exists, err := s.HasBlob(ctx, orphan.String())
	require.NoError(t, err)
	require.True(t, exists)
	pending, err := s.PendingGCLooseRetirements(ctx, 10)
	require.NoError(t, err)
	require.Empty(t, pending)
	require.NoError(t, s.DeleteBlobRowsWithGCRetirements(ctx, []string{orphan.String()}, []GCLooseRetirement{receipt}, nil))
	// Replaying the transaction after a lost reply records no duplicate work.
	require.NoError(t, s.DeleteBlobRowsWithGCRetirements(ctx, []string{orphan.String()}, []GCLooseRetirement{receipt}, nil))
	pending, err = s.PendingGCLooseRetirements(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, []GCLooseRetirement{receipt}, pending)
	for range 2 {
		retire, err := s.PrepareGCLooseRetirement(ctx, receipt)
		require.NoError(t, err)
		require.True(t, retire, "failed physical cleanup remains retryable")
	}
	// A new native ingest reauthorizes the same loose bytes. Retrying the old
	// cleanup consumes its receipt without authorizing deletion.
	_, err = s.CreateFile(ctx, s.RootID(), "returned.txt", orphan.String(), 13, "text/plain")
	require.NoError(t, err)
	retire, err := s.PrepareGCLooseRetirement(ctx, receipt)
	require.NoError(t, err)
	require.False(t, retire)
	pending, err = s.PendingGCLooseRetirements(ctx, 10)
	require.NoError(t, err)
	require.Empty(t, pending)
	authorized, err := s.HasPrimaryLooseAuthority(ctx, orphan.String())
	require.NoError(t, err)
	require.True(t, authorized)
	// Completing one encoding does not consume a different exact object.
	other, err := packstore.ParseHash(fakeHash("cc"))
	require.NoError(t, err)
	h.SetMember(other, true)
	raw := GCLooseRetirement{StoreID: s.primaryStoreID, Hash: other, Encoding: packstore.LooseEncodingRaw}
	compressed := raw
	compressed.Encoding = packstore.LooseEncodingZstd
	require.NoError(t, s.DeleteBlobRowsWithGCRetirements(ctx, []string{other.String()}, []GCLooseRetirement{raw, compressed}, nil))
	require.NoError(t, s.CompleteGCLooseRetirement(ctx, raw))
	pending, err = s.PendingGCLooseRetirements(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, []GCLooseRetirement{compressed}, pending)
	require.ErrorIs(t, s.CompleteGCLooseRetirement(ctx, raw), ErrNotFound)
	require.NoError(t, s.CompleteGCLooseRetirement(ctx, compressed))
	checkRetentionMetadata(t, s)
}

func checkPackSummaryMutationsPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	h := &docbankPackHarness{t: t, store: s}
	catalog := NewPackCatalog(s)
	a, err := packstore.ParseHash(fakeHash("aa"))
	require.NoError(t, err)
	b, err := packstore.ParseHash(fakeHash("bb"))
	require.NoError(t, err)
	h.SetMember(a, true)
	h.SetMember(b, true)
	first, second := pack.NewPackID(), pack.NewPackID()
	now := time.Now().UTC()
	entry := packstore.IndexEntry{Hash: a, PackID: first, Offset: pack.MinEntryOffset, StoredLen: 5, RawLen: 13}
	h.PutPack(packstore.PackRecord{PackID: first, EntryCount: 2, StoredBytes: 14, CreatedAt: now}, []packstore.IndexEntry{
		entry, {Hash: b, PackID: first, Offset: pack.MinEntryOffset + 32, StoredLen: 9, RawLen: 13},
	})
	entry.PackID = second
	require.NoError(t, catalog.AdoptPack(ctx, packstore.PackRecord{PackID: second, EntryCount: 1, StoredBytes: 5, CreatedAt: now.Add(time.Second)}, []packstore.Adoption{{Entry: entry}}))
	usage, err := catalog.ListPackUsage(ctx)
	require.NoError(t, err)
	require.Len(t, usage, 2)
	require.EqualValues(t, 1, usage[0].LiveEntries)
	require.EqualValues(t, 9, usage[0].LiveStoredBytes)
	require.EqualValues(t, 9, usage[0].MaxLiveStoredLen)
	require.EqualValues(t, 1, usage[1].LiveEntries)
	require.EqualValues(t, 5, usage[1].MaxLiveStoredLen)
	require.NoError(t, catalog.DeleteIndexEntry(ctx, b))
	refs, err := s.RepairBlobAuthority(ctx, a.String(), 13, BlobPhysical{Encoding: "raw", StoredBytes: 13, PackEligible: true})
	require.NoError(t, err)
	require.Zero(t, refs)
	usage, err = catalog.ListPackUsage(ctx)
	require.NoError(t, err)
	for _, item := range usage {
		require.Zero(t, item.LiveEntries)
		require.Zero(t, item.LiveStoredBytes)
		require.Zero(t, item.MaxLiveStoredLen)
	}
	var key string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT scan_hash FROM blob_packs WHERE store_id=$1 AND pack_id=$2`, s.primaryStoreID, first).Scan(&key))
	require.Equal(t, a.String(), key, "removing the lowest mapping must not change a maintenance cursor")
	dead, more, err := s.DeadPackUsagePage(ctx, 1)
	require.NoError(t, err)
	require.True(t, more)
	require.Len(t, dead, 1)
	removed, err := catalog.DeleteEmptyPackRecord(ctx, dead[0].PackID)
	require.NoError(t, err)
	require.True(t, removed)
	dead, more, err = s.DeadPackUsagePage(ctx, 1)
	require.NoError(t, err)
	require.False(t, more)
	require.Len(t, dead, 1)
	backlog, err := s.LooseBacklog(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, backlog.EligibleObjects)
	require.EqualValues(t, 13, backlog.EligibleBytes)
}

func checkPhysicalMaintenancePagesPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	h := &docbankPackHarness{t: t, store: s}
	hashes := make([]packstore.Hash, 4)
	entries := make([]packstore.IndexEntry, 4)
	packID := pack.NewPackID()
	for i := range hashes {
		var err error
		hashes[i], err = packstore.ParseHash(fmt.Sprintf("%064x", i+1))
		require.NoError(t, err)
		if i < 2 {
			_, err = s.CreateFile(ctx, s.RootID(), fmt.Sprintf("retained-%d.txt", i), hashes[i].String(), 13, "text/plain")
			require.NoError(t, err)
		} else {
			h.SetMember(hashes[i], true)
		}
		entries[i] = packstore.IndexEntry{Hash: hashes[i], PackID: packID, Offset: pack.MinEntryOffset + int64(i)*32, StoredLen: 5, RawLen: 13}
	}
	h.PutPack(packstore.PackRecord{PackID: packID, EntryCount: 4, StoredBytes: 20, CreatedAt: time.Now().UTC().Add(-48 * time.Hour)}, entries)
	first, err := s.UnreachableBlobsPageFrom(ctx, nil, 2)
	require.NoError(t, err)
	require.Empty(t, first.Items)
	require.True(t, first.More)
	require.Equal(t, 2, first.Examined)
	require.Equal(t, hashes[1].String(), first.HighWater)
	second, err := s.UnreachableBlobsPageFrom(ctx, &first.HighWater, 2)
	require.NoError(t, err)
	require.False(t, second.More)
	require.Equal(t, []GCCandidate{{Hash: hashes[2].String()}, {Hash: hashes[3].String()}}, second.Items)
	inventory, more, err := s.BlobHashesPageFrom(ctx, nil, 2)
	require.NoError(t, err)
	require.True(t, more)
	require.Equal(t, []string{hashes[0].String(), hashes[1].String()}, inventory)
	blobs, more, err := s.BlobsPage(ctx, inventory[1], 2)
	require.NoError(t, err)
	require.False(t, more)
	require.Equal(t, []BlobInfo{{Hash: hashes[2].String(), Size: 13}, {Hash: hashes[3].String(), Size: 13}}, blobs)
	require.NoError(t, s.DeleteBlobRows(ctx, []string{hashes[2].String(), hashes[3].String()}))
	mappings, err := s.UnreferencedPackMappingsPage(ctx, nil, 2)
	require.NoError(t, err)
	require.Empty(t, mappings.Items)
	require.True(t, mappings.More)
	require.Equal(t, 2, mappings.Examined)
	mappings, err = s.UnreferencedPackMappingsPage(ctx, &mappings.HighWater, 2)
	require.NoError(t, err)
	require.Equal(t, []string{hashes[2].String(), hashes[3].String()}, mappings.Items)
	removed, err := s.DeleteUnreferencedPackMappings(ctx, []string{hashes[0].String(), hashes[2].String()})
	require.NoError(t, err)
	require.EqualValues(t, 1, removed, "current membership protects a mapping even if selection is stale")
	sparse, err := s.SparseRepackScanPage(ctx, "", "", 1, time.Now().UTC(), time.Second, 1)
	require.NoError(t, err)
	require.Len(t, sparse.Items, 1)
	require.True(t, sparse.Items[0].Eligible)
	require.EqualValues(t, 2, sparse.Items[0].Usage.LiveEntries)
	require.Equal(t, hashes[0].String(), sparse.Items[0].Hash)
	after, more, err := s.SparseRepackPage(ctx, sparse.Items[0].Hash, 1, time.Now().UTC(), time.Second, 1)
	require.NoError(t, err)
	require.False(t, more)
	require.Empty(t, after, "the hash cursor skips the previously visited pack")
}
