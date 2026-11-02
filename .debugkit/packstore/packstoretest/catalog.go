// Package packstoretest provides conformance tests for application catalog
// adapters. Applications run the same suite against their real database
// implementations that Kit runs against its in-memory test catalog.
package packstoretest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

// CatalogHarness gives the conformance suite product-neutral fixture control.
// Harness methods are test setup/inspection operations, not production catalog
// API and may use application-specific database helpers internally.
type CatalogHarness interface {
	Catalog() packstore.Catalog
	SetMember(packstore.Hash, bool)
	SetCandidate(packstore.Candidate)
	PutPack(packstore.PackRecord, []packstore.IndexEntry)
	Snapshot() CatalogState
}

// CatalogState is the observable storage-authority state after a transaction.
type CatalogState struct {
	Members map[packstore.Hash]bool
	Entries map[packstore.Hash]packstore.IndexEntry
	Packs   map[string]packstore.PackRecord
}

// ContractOptions supplies deterministic values for a conformance run.
type ContractOptions struct {
	Now       time.Time
	NewPackID func() string
}

// RunCatalogContract exercises the semantic operations required by the Kit
// maintenance engine. The factory must return isolated state for every call.
func RunCatalogContract(t *testing.T, factory func(*testing.T) CatalogHarness, opts ContractOptions) {
	t.Helper()
	require.False(t, opts.Now.IsZero())
	require.NotNil(t, opts.NewPackID)

	hashA := packstore.Hash(pack.ComputeBlobID([]byte("catalog-alpha")).String())
	hashB := packstore.Hash(pack.ComputeBlobID([]byte("catalog-bravo")).String())

	t.Run("resolution follows membership and mapping", func(t *testing.T) {
		h := factory(t)
		ctx := t.Context()
		h.SetMember(hashA, true)

		loc, err := h.Catalog().Resolve(ctx, hashA)
		require.NoError(t, err)
		assert.True(t, loc.Member)
		assert.Nil(t, loc.Pack)

		packID := opts.NewPackID()
		entry := contractEntry(hashA, packID, 6, 13)
		h.PutPack(contractRecord(packID, 1, entry.StoredLen, opts.Now), []packstore.IndexEntry{entry})
		loc, err = h.Catalog().Resolve(ctx, hashA)
		require.NoError(t, err)
		require.NotNil(t, loc.Pack)
		assert.Equal(t, entry, *loc.Pack)

		h.SetMember(hashA, false)
		loc, err = h.Catalog().Resolve(ctx, hashA)
		require.NoError(t, err)
		assert.False(t, loc.Member)
		assert.Nil(t, loc.Pack)
	})

	t.Run("inventory separates unpacked and indexed members", func(t *testing.T) {
		h := factory(t)
		ctx := t.Context()
		h.SetMember(hashA, true)
		h.SetMember(hashB, true)
		candidate := packstore.Candidate{
			Hash:           hashA,
			OriginalHashes: []string{hashA.String()},
			Paths:          []string{hashA.String()[:2] + "/" + hashA.String()},
			Size:           13,
		}
		h.SetCandidate(candidate)
		packID := opts.NewPackID()
		entry := contractEntry(hashB, packID, 6, 13)
		h.PutPack(contractRecord(packID, 1, entry.StoredLen, opts.Now), []packstore.IndexEntry{entry})

		inventory, err := h.Catalog().ListReferences(ctx)
		require.NoError(t, err)
		assert.True(t, inventory.Complete)
		assert.ElementsMatch(t, []packstore.Hash{hashA, hashB}, referenceHashes(inventory.References))
		candidates, err := h.Catalog().ListUnpacked(ctx)
		require.NoError(t, err)
		assert.Equal(t, []packstore.Candidate{candidate}, candidates)
		indexed, err := h.Catalog().ListIndexed(ctx)
		require.NoError(t, err)
		assert.Equal(t, []packstore.IndexEntry{entry}, indexed)
		records, err := h.Catalog().ListPackRecords(ctx)
		require.NoError(t, err)
		assert.Equal(t, []packstore.PackRecord{contractRecord(packID, 1, entry.StoredLen, opts.Now)}, records)
		entries, err := h.Catalog().ListPackEntries(ctx, packID)
		require.NoError(t, err)
		assert.Equal(t, []packstore.IndexEntry{entry}, entries)
		has, err := h.Catalog().HasPackRecord(ctx, packID)
		require.NoError(t, err)
		assert.True(t, has)
	})

	t.Run("record adopt and prune preserve authority", func(t *testing.T) {
		h := factory(t)
		ctx := t.Context()
		h.SetMember(hashA, true)
		h.SetMember(hashB, true)

		firstID := opts.NewPackID()
		firstEntry := contractEntry(hashA, firstID, 6, 13)
		require.NoError(t, h.Catalog().RecordPack(ctx,
			contractRecord(firstID, 1, firstEntry.StoredLen, opts.Now),
			[]packstore.Adoption{{Entry: firstEntry, OriginalHashes: []string{hashA.String()}}}))
		assert.Equal(t, firstEntry, h.Snapshot().Entries[hashA])

		adoptedID := opts.NewPackID()
		adoptedEntry := contractEntry(hashA, adoptedID, 6, 13)
		require.NoError(t, h.Catalog().AdoptPack(ctx,
			contractRecord(adoptedID, 1, adoptedEntry.StoredLen, opts.Now),
			[]packstore.Adoption{{Entry: adoptedEntry, OriginalHashes: []string{hashA.String()}}}))
		assert.Equal(t, adoptedEntry, h.Snapshot().Entries[hashA])

		deadID := opts.NewPackID()
		deadEntry := contractEntry(hashB, deadID, 6, 13)
		h.PutPack(contractRecord(deadID, 1, deadEntry.StoredLen, opts.Now), []packstore.IndexEntry{deadEntry})
		h.SetMember(hashB, false)
		pruned, err := h.Catalog().PruneUnreferenced(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(1), pruned)
		_, remains := h.Snapshot().Entries[hashB]
		assert.False(t, remains)
	})

	t.Run("repack is exact and old record retires only when empty", func(t *testing.T) {
		h := factory(t)
		ctx := t.Context()
		h.SetMember(hashA, true)
		oldID := opts.NewPackID()
		oldEntry := contractEntry(hashA, oldID, 6, 13)
		h.PutPack(contractRecord(oldID, 1, oldEntry.StoredLen, opts.Now), []packstore.IndexEntry{oldEntry})

		usage, err := h.Catalog().ListPackUsage(ctx)
		require.NoError(t, err)
		require.Len(t, usage, 1)
		assert.Equal(t, int64(1), usage[0].LiveEntries)
		live, err := h.Catalog().ListLivePackEntries(ctx, oldID)
		require.NoError(t, err)
		assert.Equal(t, []packstore.IndexEntry{oldEntry}, live)

		newID := opts.NewPackID()
		newEntry := contractEntry(hashA, newID, 6, 13)
		newRecord := contractRecord(newID, 1, newEntry.StoredLen, opts.Now.Add(time.Second))
		beforeInvalid := h.Snapshot()
		err = h.Catalog().CommitRepack(ctx, []string{oldID}, []packstore.PackRecord{newRecord}, nil)
		require.Error(t, err)
		assert.Equal(t, beforeInvalid, h.Snapshot(), "failed repack must be transactionally unchanged")
		require.NoError(t, h.Catalog().CommitRepack(ctx, []string{oldID}, []packstore.PackRecord{newRecord},
			[]packstore.RepackMove{{OldPackID: oldID, NewEntry: newEntry}}))
		assert.Equal(t, newEntry, h.Snapshot().Entries[hashA])

		deleted, err := h.Catalog().DeleteEmptyPackRecord(ctx, oldID)
		require.NoError(t, err)
		assert.True(t, deleted)
		_, remains := h.Snapshot().Packs[oldID]
		assert.False(t, remains)
	})

	t.Run("invalid repack variants are transactionally unchanged", func(t *testing.T) {
		cases := []struct {
			name              string
			membershipChanged bool
			moves             func(oldID, newID string, oldEntry, newEntry packstore.IndexEntry) []packstore.RepackMove
		}{
			{name: "omitted", moves: func(_, _ string, _, _ packstore.IndexEntry) []packstore.RepackMove { return nil }},
			{name: "duplicate", moves: func(oldID, _ string, _, newEntry packstore.IndexEntry) []packstore.RepackMove {
				move := packstore.RepackMove{OldPackID: oldID, NewEntry: newEntry}
				return []packstore.RepackMove{move, move}
			}},
			{name: "extra", moves: func(oldID, newID string, _, newEntry packstore.IndexEntry) []packstore.RepackMove {
				extra := contractEntry(hashB, newID, 19, 13)
				return []packstore.RepackMove{{OldPackID: oldID, NewEntry: newEntry}, {OldPackID: oldID, NewEntry: extra}}
			}},
			{name: "wrong source", moves: func(_, _ string, _, newEntry packstore.IndexEntry) []packstore.RepackMove {
				return []packstore.RepackMove{{OldPackID: opts.NewPackID(), NewEntry: newEntry}}
			}},
			{name: "membership changed", membershipChanged: true, moves: func(oldID, _ string, _, newEntry packstore.IndexEntry) []packstore.RepackMove {
				return []packstore.RepackMove{{OldPackID: oldID, NewEntry: newEntry}}
			}},
		}
		for _, tt := range cases {
			t.Run(tt.name, func(t *testing.T) {
				h := factory(t)
				ctx := t.Context()
				h.SetMember(hashA, true)
				oldID := opts.NewPackID()
				oldEntry := contractEntry(hashA, oldID, 6, 13)
				h.PutPack(contractRecord(oldID, 1, oldEntry.StoredLen, opts.Now), []packstore.IndexEntry{oldEntry})
				if tt.membershipChanged {
					h.SetMember(hashA, false)
				}
				newID := opts.NewPackID()
				newEntry := contractEntry(hashA, newID, 6, 13)
				newRecord := contractRecord(newID, 1, newEntry.StoredLen, opts.Now.Add(time.Second))
				before := h.Snapshot()
				err := h.Catalog().CommitRepack(ctx, []string{oldID}, []packstore.PackRecord{newRecord},
					tt.moves(oldID, newID, oldEntry, newEntry))
				require.Error(t, err)
				assert.Equal(t, before, h.Snapshot())
			})
		}
	})

	t.Run("explicit deletion and reset remove packed authority", func(t *testing.T) {
		h := factory(t)
		ctx := t.Context()
		h.SetMember(hashA, true)
		h.SetMember(hashB, true)
		packID := opts.NewPackID()
		entryA := contractEntry(hashA, packID, 6, 13)
		entryB := contractEntry(hashB, packID, 19, 13)
		h.PutPack(contractRecord(packID, 2, 26, opts.Now), []packstore.IndexEntry{entryA, entryB})

		require.NoError(t, h.Catalog().DeleteIndexEntry(ctx, hashA))
		_, remains := h.Snapshot().Entries[hashA]
		assert.False(t, remains)
		require.NoError(t, h.Catalog().DeletePackRecord(ctx, packID))
		assert.Empty(t, h.Snapshot().Entries)
		assert.Empty(t, h.Snapshot().Packs)

		secondID := opts.NewPackID()
		h.PutPack(contractRecord(secondID, 1, entryA.StoredLen, opts.Now),
			[]packstore.IndexEntry{contractEntry(hashA, secondID, 6, 13)})
		require.NoError(t, h.Catalog().ClearPackMetadata(ctx))
		assert.Empty(t, h.Snapshot().Entries)
		assert.Empty(t, h.Snapshot().Packs)
	})
}

func contractEntry(hash packstore.Hash, packID string, offset, size int64) packstore.IndexEntry {
	return packstore.IndexEntry{Hash: hash, PackID: packID, Offset: offset, StoredLen: size, RawLen: size}
}

func contractRecord(packID string, count, size int64, now time.Time) packstore.PackRecord {
	return packstore.PackRecord{PackID: packID, EntryCount: count, StoredBytes: size, CreatedAt: now}
}

func referenceHashes(refs []packstore.Reference) []packstore.Hash {
	hashes := make([]packstore.Hash, len(refs))
	for i, ref := range refs {
		hashes[i] = ref.Hash
	}
	return hashes
}
