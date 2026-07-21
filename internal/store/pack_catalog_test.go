package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
	"go.kenn.io/kit/packstore/packstoretest"
)

func TestPackCatalogContract(t *testing.T) {
	packstoretest.RunCatalogContract(t, newDocbankPackHarness, packstoretest.ContractOptions{
		Now:       time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC),
		NewPackID: pack.NewPackID,
	})
}

func TestPackAdoptionClearsLooseAuthority(t *testing.T) {
	s := newTestStore(t)
	hash, err := packstore.ParseHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.NoError(t, err)
	_, err = s.CreateFile(t.Context(), s.RootID(), "compressed.txt", hash.String(), 20, "text/plain",
		BlobPhysical{Encoding: "zstd", StoredBytes: 9, PackEligible: true})
	require.NoError(t, err)

	packID := pack.NewPackID()
	record := packstore.PackRecord{
		PackID: packID, EntryCount: 1, StoredBytes: 32,
		CreatedAt: time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC),
	}
	entry := packstore.IndexEntry{
		Hash: hash, PackID: packID, Offset: pack.MinEntryOffset,
		StoredLen: 9, RawLen: 20,
	}
	require.NoError(t, NewPackCatalog(s).RecordPack(t.Context(), record, []packstore.Adoption{{
		Entry: entry, OriginalHashes: []string{hash.String()},
	}}))

	physical, err := s.PhysicalContent(t.Context(), hash.String())
	require.NoError(t, err)
	assert.Equal(t, PhysicalContent{
		Kind: "packed", Encoding: "raw", LogicalBytes: 20, StoredBytes: 9, PackEligible: true,
	}, physical)
	var encoding, stored any
	require.NoError(t, s.db.QueryRow(`SELECT loose_encoding, loose_stored_size FROM blobs WHERE hash = ?`,
		hash.String()).Scan(&encoding, &stored))
	assert.Nil(t, encoding)
	assert.Nil(t, stored)

	// A later logical reference through the legacy raw-default API must not
	// overwrite the existing packed authority.
	_, err = s.CreateFile(t.Context(), s.RootID(), "same-content.txt", hash.String(), 20, "text/plain")
	require.NoError(t, err)
	after, err := s.PhysicalContent(t.Context(), hash.String())
	require.NoError(t, err)
	assert.Equal(t, physical, after)
}

func TestRepairBlobAuthorityPreservesReferences(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	hash := fakeHash("a11ce")
	replacementHash := fakeHash("b0b")
	first, err := s.CreateFile(ctx, s.RootID(), "first.txt", hash, 20, "text/plain",
		BlobPhysical{Encoding: "raw", StoredBytes: 20, PackEligible: true})
	require.NoError(t, err)
	_, _, err = s.ReplaceContent(ctx, first.ID, first.Revision, replacementHash, 9, "text/plain",
		BlobPhysical{Encoding: "raw", StoredBytes: 9, PackEligible: true})
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, s.RootID(), "second.txt", hash, 20, "text/plain")
	require.NoError(t, err)

	parsed, err := packstore.ParseHash(hash)
	require.NoError(t, err)
	packID := pack.NewPackID()
	require.NoError(t, NewPackCatalog(s).RecordPack(ctx, packstore.PackRecord{
		PackID: packID, EntryCount: 1, StoredBytes: 32,
		CreatedAt: time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC),
	}, []packstore.Adoption{{Entry: packstore.IndexEntry{
		Hash: parsed, PackID: packID, Offset: pack.MinEntryOffset,
		StoredLen: 9, RawLen: 20,
	}}}))

	_, err = s.RepairBlobAuthority(ctx, hash, 21,
		BlobPhysical{Encoding: "zstd", StoredBytes: 8, PackEligible: true})
	require.Error(t, err)
	physical, err := s.PhysicalContent(ctx, hash)
	require.NoError(t, err)
	assert.Equal(t, "packed", physical.Kind)

	references, err := s.RepairBlobAuthority(ctx, hash, 20,
		BlobPhysical{Encoding: "zstd", StoredBytes: 8, PackEligible: true})
	require.NoError(t, err)
	assert.Equal(t, int64(2), references)
	physical, err = s.PhysicalContent(ctx, hash)
	require.NoError(t, err)
	assert.Equal(t, PhysicalContent{
		Kind: "loose", Encoding: "zstd", LogicalBytes: 20,
		StoredBytes: 8, PackEligible: true,
	}, physical)

	var versions int64
	require.NoError(t, s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM content_versions WHERE blob_hash = ?`, hash).Scan(&versions))
	assert.Equal(t, int64(2), versions)
	var mappings int64
	require.NoError(t, s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM blob_pack_index WHERE blob_hash = ?`, hash).Scan(&mappings))
	assert.Zero(t, mappings)
}

func TestRepairBlobAuthorityRequiresExistingMembership(t *testing.T) {
	s := newTestStore(t)
	_, err := s.RepairBlobAuthority(t.Context(), fakeHash("missing"), 10,
		BlobPhysical{Encoding: "raw", StoredBytes: 10, PackEligible: true})
	require.ErrorIs(t, err, ErrNotFound)
}

type docbankPackHarness struct {
	t     *testing.T
	store *Store
}

func newDocbankPackHarness(t *testing.T) packstoretest.CatalogHarness {
	t.Helper()
	return &docbankPackHarness{t: t, store: newTestStore(t)}
}

func (h *docbankPackHarness) Catalog() packstore.Catalog { return NewPackCatalog(h.store) }

func (h *docbankPackHarness) SetMember(hash packstore.Hash, member bool) {
	h.t.Helper()
	if member {
		_, err := h.store.db.Exec(`INSERT OR IGNORE INTO blobs
			(hash, size, created_at, loose_encoding, loose_stored_size, pack_eligible)
			VALUES (?, 13, ?, 'raw', 13, 1)`,
			hash.String(), nowRFC3339())
		require.NoError(h.t, err)
		return
	}
	_, err := h.store.db.Exec(`DELETE FROM blobs WHERE hash = ?`, hash.String())
	require.NoError(h.t, err)
}

func (h *docbankPackHarness) SetCandidate(candidate packstore.Candidate) {
	h.t.Helper()
	_, err := h.store.db.Exec(`UPDATE blobs SET size = ?, loose_stored_size = ?,
		pack_eligible = CASE WHEN ? <= ? THEN 1 ELSE 0 END WHERE hash = ?`,
		candidate.Size, candidate.Size, candidate.Size, maxPackEligibleBytes, candidate.Hash.String())
	require.NoError(h.t, err)
}

func (h *docbankPackHarness) PutPack(record packstore.PackRecord, entries []packstore.IndexEntry) {
	h.t.Helper()
	_, err := h.store.db.Exec(`
		INSERT INTO blob_packs (pack_id, entry_count, stored_bytes, created_at) VALUES (?, ?, ?, ?)`,
		record.PackID, record.EntryCount, record.StoredBytes, record.CreatedAt.UTC().Format(timestampLayout))
	require.NoError(h.t, err)
	for _, entry := range entries {
		_, err := h.store.db.Exec(`
			INSERT INTO blob_pack_index
				(blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, entry.Hash.String(), entry.PackID, entry.Offset,
			entry.StoredLen, entry.RawLen, entry.Flags, entry.CRC32C)
		require.NoError(h.t, err)
	}
}

func (h *docbankPackHarness) Snapshot() packstoretest.CatalogState {
	h.t.Helper()
	ctx := context.Background()
	state := packstoretest.CatalogState{
		Members: make(map[packstore.Hash]bool),
		Entries: make(map[packstore.Hash]packstore.IndexEntry),
		Packs:   make(map[string]packstore.PackRecord),
	}
	refs, err := NewPackCatalog(h.store).ListReferences(ctx)
	require.NoError(h.t, err)
	for _, ref := range refs.References {
		state.Members[ref.Hash] = true
	}
	entries, err := NewPackCatalog(h.store).ListIndexed(ctx)
	require.NoError(h.t, err)
	for _, entry := range entries {
		state.Entries[entry.Hash] = entry
	}
	records, err := NewPackCatalog(h.store).ListPackRecords(ctx)
	require.NoError(h.t, err)
	for _, record := range records {
		state.Packs[record.PackID] = record
	}
	return state
}
