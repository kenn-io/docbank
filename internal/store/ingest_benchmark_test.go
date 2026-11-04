package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkIngestFolderSize measures committed metadata writes, excluding blob
// I/O and fixture creation, as the destination folder grows.
func BenchmarkIngestFolderSize(b *testing.B) {
	for _, kind := range []string{"cli", "photo-import"} {
		for _, siblings := range []int{1000, 10000, 100000} {
			b.Run(fmt.Sprintf("%s/siblings=%d", kind, siblings), func(b *testing.B) {
				s, err := Open(b.Context(), filepath.Join(b.TempDir(), "docbank.db"))
				require.NoError(b, err)
				b.Cleanup(func() { require.NoError(b, s.Close()) })
				require.NoError(b, s.withStorageTx(b.Context(), func(tx *sql.Tx) error {
					for i := range siblings {
						_, _, err := s.createFileTx(b.Context(), tx, s.RootID(), fmt.Sprintf("seed-%07d.bin", i),
							fmt.Sprintf("%064x", i+1), 8192, "application/octet-stream")
						if err != nil {
							return err
						}
					}
					return nil
				}))
				run, err := s.BeginIngest(b.Context(), kind, "Synthetic corpus")
				require.NoError(b, err)
				require.NoError(b, s.Checkpoint(b.Context()))
				i := 0
				for b.Loop() {
					name := fmt.Sprintf("file-%07d.bin", i)
					hash := fmt.Sprintf("%064x", siblings+i+1)
					if kind == "cli" {
						_, added, err := s.IngestFileWithMembership(b.Context(), run, s.RootID(), name,
							hash, 8192, "application/octet-stream", "/synthetic/"+name, "")
						require.NoError(b, err)
						require.True(b, added)
					} else {
						name = fmt.Sprintf("file-%07d.jpg", i)
						result, err := s.IngestPhotoGroup(b.Context(), run, PhotoImportGroup{
							Members: []PhotoImportMember{{Name: name, Role: PhotoRoleImage, BlobHash: hash,
								Size: 8192, MediaType: "image/jpeg", OriginalPath: "/synthetic/" + name}},
						})
						require.NoError(b, err)
						require.True(b, result.Added)
						require.Len(b, result.Nodes, 1)
					}
					i++
				}
			})
		}
	}
}

// BenchmarkResolveIngestName isolates the work performed for every local add
// and HTTP upload as the destination directory grows. Setup uses real file
// authority but is excluded from timing.
func BenchmarkResolveIngestName(b *testing.B) {
	for _, siblings := range []int{0, 100, 1000, 10000} {
		b.Run(fmt.Sprintf("siblings=%d", siblings), func(b *testing.B) {
			s, err := Open(b.Context(), filepath.Join(b.TempDir(), "docbank.db"))
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
				name, _, skipped, err := resolveIngestNameTx(b.Context(), tx, s.RootID(), "new.bin",
					fmt.Sprintf("%064x", siblings+1), "cli", "new.bin")
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
			s, err := Open(b.Context(), filepath.Join(b.TempDir(), "docbank.db"))
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
						name, id, skipped, err := resolveIngestNameTx(b.Context(), tx, s.RootID(), tc.name, fakeHash("a1"), "cli", tc.name)
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
