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
