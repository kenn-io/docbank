package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"
)

func TestLooseBacklogUsesIndexedPhysicalState(t *testing.T) {
	s := newTestStore(t)
	created := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC).Format(timestampLayout)
	_, err := s.db.Exec(`
		INSERT INTO blobs(hash,size,created_at,loose_encoding,loose_stored_size,pack_eligible) VALUES
			('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',10,?,'raw',10,1),
			('bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',20,?,'zstd',9,1),
			('cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc',67108865,?,'zstd',1024,0),
			('dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd',40,?,NULL,NULL,1);
		INSERT INTO blob_packs(pack_id,entry_count,stored_bytes,created_at)
		VALUES('01k00000000000000000000000',1,19,?);
		INSERT INTO blob_pack_index(blob_hash,pack_id,pack_offset,stored_len,raw_len,flags,crc32c)
		VALUES('dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd',
		       '01k00000000000000000000000',0,19,40,0,0);
	`, created, created, created, created, created)
	require.NoError(t, err)

	backlog, err := s.LooseBacklog(t.Context())
	require.NoError(t, err)
	assert.Equal(t, LooseBacklog{
		EligibleObjects:   2,
		EligibleBytes:     30,
		RawObjects:        1,
		CompressedObjects: 1,
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
