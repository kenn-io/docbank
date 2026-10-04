package store

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkBlobStoreInventory measures the inventory used by info and storage
// status. The second store holds replicas of half the objects; further stores
// are empty. Catalog creation and replica placement are outside the timer.
func BenchmarkBlobStoreInventory(b *testing.B) {
	for _, files := range []int{100, 10000} {
		b.Run(fmt.Sprintf("files=%d", files), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			for i := range files {
				name := fmt.Sprintf("file-%06d.bin", i)
				hash := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))
				_, err := s.CreateFile(b.Context(), s.RootID(), name, hash, 128,
					"application/octet-stream")
				require.NoError(b, err)
			}
			registered := 1
			for _, stores := range []int{1, 2, 20} {
				for registered < stores {
					name := fmt.Sprintf("archive-%d", registered)
					secondary, err := s.PrepareSecondaryBlobStore(name, "filesystem", name)
					require.NoError(b, err)
					require.NoError(b, s.RegisterBlobStore(b.Context(), secondary))
					if registered == 1 {
						_, err = s.db.ExecContext(b.Context(), `
						INSERT INTO blob_locations
						SELECT blob_hash, ?, generation, kind, encoding, stored_size, pack_eligible
						FROM blob_locations WHERE store_id=? LIMIT ?`,
							secondary.ID, s.primaryStoreID, files/2)
						require.NoError(b, err)
					}
					registered++
				}
				b.Run(fmt.Sprintf("stores=%d", stores), func(b *testing.B) {
					affected := int64(files)
					if stores > 1 {
						affected /= 2
					}
					b.ReportAllocs()
					for b.Loop() {
						inventory, err := s.BlobStoreInventory(b.Context())
						require.NoError(b, err)
						require.Equal(b, BlobStoreStats{
							AuthoritativeObjects: int64(files), LogicalBytes: int64(files * 128),
							StoredBytes: int64(files * 128), SoleAuthorityObjects: affected,
							AffectedDocuments: affected,
						}, inventory[s.primaryStoreID])
					}
				})
			}
		})
	}
}
