package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func addTestPack(
	t *testing.T,
	s *Store,
	packID string,
	entryCount int64,
	storedBytes int64,
	createdAt string,
	scanHash ...string,
) {
	t.Helper()
	scan := ""
	if len(scanHash) > 0 {
		scan = scanHash[0]
	}
	_, err := s.db.Exec(`
		INSERT INTO blob_packs(
			store_id, pack_id, entry_count, stored_bytes, created_at, scan_hash
		) VALUES(?, ?, ?, ?, ?, ?)`,
		s.primaryStoreID, packID, entryCount, storedBytes, createdAt, scan,
	)
	require.NoError(t, err)
}

func addTestPackEntry(
	t *testing.T,
	s *Store,
	hash string,
	packID string,
	offset int64,
	storedLen int64,
	rawLen int64,
) {
	t.Helper()
	_, err := s.db.Exec(`
		INSERT INTO blob_pack_entries(
			blob_hash, store_id, pack_id, pack_offset, stored_len, raw_len, flags, crc32c
		) VALUES(?, ?, ?, ?, ?, ?, 0, 0)`,
		hash, s.primaryStoreID, packID, offset, storedLen, rawLen,
	)
	require.NoError(t, err)
}
