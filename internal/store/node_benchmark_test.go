package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkCreateFilePhotoSubscriber keeps catalog and capture setup outside the
// timer, so each operation measures one ordinary file's committed metadata write.
func BenchmarkCreateFilePhotoSubscriber(b *testing.B) {
	for _, existing := range []int{0, 100000} {
		for _, audience := range []string{"none", "locked", "hidden"} {
			b.Run(fmt.Sprintf("existing=%d/subscriber=%s", existing, audience), func(b *testing.B) {
				s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
				require.NoError(b, err)
				b.Cleanup(func() { require.NoError(b, s.Close()) })
				ctx := b.Context()
				run, err := s.BeginIngest(ctx, "cli", "Synthetic ordinary file corpus")
				require.NoError(b, err)
				require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
					for i := range existing {
						name := fmt.Sprintf("existing-%06d.bin", i)
						_, _, _, err := s.ingestFileTx(ctx, tx, run, s.RootID(), name,
							fmt.Sprintf("%064x", i+1), 4, "application/octet-stream", "/synthetic/"+name, "",
							ingestFileOptions{observeMembership: true})
						if err != nil {
							return err
						}
					}
					return nil
				}))
				ctx, cancel := setupPhotoBenchmarkCapture(b, s, audience)
				b.Cleanup(cancel)
				// Warm the measured write path in every case.
				_, err = s.CreateFile(ctx, s.RootID(), "warmup.bin", fmt.Sprintf("%064x", 0), 4, "application/octet-stream")
				require.NoError(b, err)
				require.NoError(b, s.Checkpoint(ctx))
				b.ReportAllocs()
				i := 0
				for b.Loop() {
					_, err := s.CreateFile(ctx, s.RootID(), fmt.Sprintf("file-%06d.bin", i),
						fmt.Sprintf("%064x", existing+i+1), 4, "application/octet-stream")
					require.NoError(b, err)
					i++
				}
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N), "ns/file")
			})
		}
	}
}

// BenchmarkDirectoryChildrenPage measures listing a bounded page as its
// directory grows. Creating the synthetic catalog is outside the timer.
func BenchmarkDirectoryChildrenPage(b *testing.B) {
	for _, children := range []int{100, 10000} {
		b.Run(fmt.Sprintf("children=%d", children), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			for i := range children {
				_, err := s.CreateFile(b.Context(), s.RootID(), fmt.Sprintf("file-%06d.bin", i),
					fmt.Sprintf("%064x", i+1), 8192, "application/octet-stream")
				require.NoError(b, err)
			}
			for _, offset := range slices.Compact([]int{0, children - 100}) {
				b.Run(fmt.Sprintf("offset=%d", offset), func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						page, err := s.DirectoryChildrenPage(b.Context(), s.RootID(), 100, offset)
						require.NoError(b, err)
						require.Equal(b, children, page.Total)
						require.Len(b, page.Children, 100)
					}
				})
			}
		})
	}
}
