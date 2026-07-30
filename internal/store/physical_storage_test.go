package store

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

func TestLooseBacklogUsesIndexedPhysicalState(t *testing.T) {
	s := newTestStore(t)
	created := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC).Format(timestampLayout)
	err := s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		for _, item := range []struct {
			hash     string
			size     int64
			physical BlobPhysical
		}{
			{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 10,
				BlobPhysical{Encoding: "raw", StoredBytes: 10, PackEligible: true}},
			{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", 20,
				BlobPhysical{Encoding: "zstd", StoredBytes: 9, PackEligible: true}},
			{"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", 67108865,
				BlobPhysical{Encoding: "zstd", StoredBytes: 1024, PackEligible: false}},
			{"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", 40,
				BlobPhysical{Encoding: "raw", StoredBytes: 40, PackEligible: true}},
		} {
			if _, err := tx.Exec(`INSERT INTO blobs(hash,size,created_at) VALUES(?,?,?)`,
				item.hash, item.size, created); err != nil {
				return err
			}
			if err := writeLooseLocationTx(
				t.Context(), tx, s.primaryStoreID, item.hash, item.physical,
			); err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)
	packedHash, err := packstore.ParseHash(
		"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
	)
	require.NoError(t, err)
	require.NoError(t, NewPackCatalog(s).RecordPack(t.Context(), packstore.PackRecord{
		PackID: "01k00000000000000000000000", EntryCount: 1, StoredBytes: 19,
		CreatedAt: time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC),
	}, []packstore.Adoption{{Entry: packstore.IndexEntry{
		Hash: packedHash, PackID: "01k00000000000000000000000",
		Offset: pack.MinEntryOffset, StoredLen: 19, RawLen: 40,
	}}}))

	backlog, err := s.LooseBacklog(t.Context())
	require.NoError(t, err)
	assert.Equal(t, LooseBacklog{
		EligibleObjects:     2,
		EligibleBytes:       30,
		EligibleStoredBytes: 19,
		RawObjects:          1,
		CompressedObjects:   1,
	}, backlog)

	candidates, err := NewPackCatalog(s).ListUnpacked(t.Context())
	require.NoError(t, err)
	require.Len(t, candidates, 2)
	assert.Equal(t, packstore.Candidate{
		Hash:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		OriginalHashes: []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		Paths:          []string{"aa/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		Size:           10,
	}, candidates[0])
	assert.Equal(t, packstore.Candidate{
		Hash:           "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		OriginalHashes: []string{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		Paths:          []string{"bb/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.zst"},
		Size:           20,
	}, candidates[1])
}
