package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

// BenchmarkListPackUsage measures reporting one pack as its blob count grows.
// Catalog creation is excluded from the timer.
func BenchmarkListPackUsage(b *testing.B) {
	for _, entries := range []int{100, 10000, 100000} {
		b.Run(fmt.Sprintf("blobs=%d", entries), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			catalog := NewPackCatalog(s)
			record := packstore.PackRecord{PackID: pack.NewPackID(), EntryCount: int64(entries),
				StoredBytes: int64(entries * 128), CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
			adoptions := make([]packstore.Adoption, entries)
			require.NoError(b, s.withStorageTx(b.Context(), func(tx *sql.Tx) error {
				for i := range entries {
					hash, err := packstore.ParseHash(fmt.Sprintf("%064x", i+1))
					if err != nil {
						return fmt.Errorf("creating benchmark hash: %w", err)
					}
					if err := s.EnsureBlobTx(tx, hash.String(), 128); err != nil {
						return err
					}
					adoptions[i].Entry = packstore.IndexEntry{Hash: hash, PackID: record.PackID,
						Offset: pack.MinEntryOffset + int64(i*128), StoredLen: 128, RawLen: 128}
				}
				return nil
			}))
			require.NoError(b, catalog.RecordPack(b.Context(), record, adoptions))
			b.ReportAllocs()
			for b.Loop() {
				usage, err := catalog.ListPackUsage(b.Context())
				require.NoError(b, err)
				require.Len(b, usage, 1)
				require.Equal(b, int64(entries), usage[0].LiveEntries)
				require.Equal(b, int64(entries*128), usage[0].LiveStoredBytes)
			}
		})
	}
}
