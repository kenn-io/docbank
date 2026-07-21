package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"

	docsqlite "go.kenn.io/docbank/pkg/sqlite"
)

func TestStorageMigrationBackfillsLegacyLooseAndPackedAuthority(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	driver := DefaultSQLiteDriver()
	db, err := driver.Open(dbPath, docsqlite.OpenOptions{
		Access: docsqlite.Create, TransactionMode: docsqlite.Immediate,
	})
	require.NoError(t, err)
	_, err = db.Exec(`
		CREATE TABLE blobs (hash TEXT PRIMARY KEY, size INTEGER NOT NULL, created_at TEXT NOT NULL);
		CREATE TABLE blob_packs (
			pack_id TEXT PRIMARY KEY, entry_count INTEGER NOT NULL,
			stored_bytes INTEGER NOT NULL, created_at TEXT NOT NULL
		);
		CREATE TABLE blob_pack_index (
			blob_hash TEXT PRIMARY KEY, pack_id TEXT NOT NULL, pack_offset INTEGER NOT NULL,
			stored_len INTEGER NOT NULL, raw_len INTEGER NOT NULL, flags INTEGER NOT NULL,
			crc32c INTEGER NOT NULL
		);
		INSERT INTO blobs(hash,size,created_at) VALUES
			('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 10, '2026-07-20T00:00:00Z'),
			('bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', 67108865, '2026-07-20T00:00:00Z'),
			('cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc', 30, '2026-07-20T00:00:00Z');
		INSERT INTO blob_packs(pack_id,entry_count,stored_bytes,created_at)
		VALUES('01k00000000000000000000000',1,17,'2026-07-20T00:00:00Z');
		INSERT INTO blob_pack_index(blob_hash,pack_id,pack_offset,stored_len,raw_len,flags,crc32c)
		VALUES('cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc',
		       '01k00000000000000000000000',0,17,30,0,0);
	`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	s, err := Open(dbPath, driver)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	raw, err := s.PhysicalContent(t.Context(), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.NoError(t, err)
	assert.Equal(t, PhysicalContent{
		Kind: "loose", Encoding: "raw", LogicalBytes: 10, StoredBytes: 10, PackEligible: true,
	}, raw)

	oversized, err := s.PhysicalContent(t.Context(), "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	require.NoError(t, err)
	assert.Equal(t, PhysicalContent{
		Kind: "loose", Encoding: "raw", LogicalBytes: 67108865,
		StoredBytes: 67108865, PackEligible: false,
	}, oversized)

	packed, err := s.PhysicalContent(t.Context(), "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	require.NoError(t, err)
	assert.Equal(t, PhysicalContent{
		Kind: "packed", Encoding: "raw", LogicalBytes: 30, StoredBytes: 17, PackEligible: true,
	}, packed)
}

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
