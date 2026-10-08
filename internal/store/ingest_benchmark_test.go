package store

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkResolveIngestName isolates the work performed for every local add
// and HTTP upload as the destination directory grows. Setup uses real file
// authority but is excluded from timing.
func BenchmarkResolveIngestName(b *testing.B) {
	for _, siblings := range []int{0, 100, 1000, 10000} {
		b.Run(fmt.Sprintf("siblings=%d", siblings), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			for i := range siblings {
				_, err := s.CreateFile(b.Context(), s.RootID(), fmt.Sprintf("file-%06d.bin", i),
					fmt.Sprintf("%064x", i+1), 4096, "application/octet-stream")
				require.NoError(b, err)
			}
			tx, err := s.writeDB.BeginTx(b.Context(), nil)
			require.NoError(b, err)
			defer func() { require.NoError(b, tx.Rollback()) }()
			b.ReportAllocs()
			for b.Loop() {
				name, _, skipped, err := resolveIngestNameTx(tx, s.RootID(), "new.bin",
					fmt.Sprintf("%064x", siblings+1), "cli")
				require.NoError(b, err)
				require.False(b, skipped)
				require.Equal(b, "new.bin", name)
			}
		})
	}
}

// Identical bytes under distinct names are separate documents. Resolving a
// new name or reimport must check their origins without one query per sibling.
func BenchmarkResolveIngestNameSharedContent(b *testing.B) {
	for _, siblings := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("siblings=%d", siblings), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			run, err := s.BeginIngest(b.Context(), "cli", "Synthetic identical files")
			require.NoError(b, err)
			var first, last Node
			for i := range siblings {
				name := fmt.Sprintf("file-%06d.bin", i)
				node, err := s.IngestFileExact(b.Context(), run, s.RootID(), name,
					fakeHash("a1"), 1024, "application/octet-stream", "/synthetic/"+name, "")
				require.NoError(b, err)
				if i == 0 {
					first = node
				}
				last = node
			}
			tx, err := s.writeDB.BeginTx(b.Context(), nil)
			require.NoError(b, err)
			defer func() { require.NoError(b, tx.Rollback()) }()
			for _, tc := range []struct {
				label, name string
				existingID  int64
			}{
				{"new", "new.bin", 0},
				{"first", first.Name, first.ID},
				{"last", last.Name, last.ID},
			} {
				b.Run(tc.label, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						name, id, skipped, err := resolveIngestNameTx(tx, s.RootID(), tc.name, fakeHash("a1"), "cli")
						if err != nil || id != tc.existingID || skipped != (tc.existingID != 0) ||
							(!skipped && name != tc.name) {
							b.Fatalf("resolved name=%q id=%d skipped=%v err=%v", name, id, skipped, err)
						}
					}
				})
			}
		})
	}
}
