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
	benchmarkCreateFilePhotoSubscriber(b, false, 0)
}

func BenchmarkCreateFilePhotoSubscriberSharedBlob(b *testing.B) {
	benchmarkCreateFilePhotoSubscriber(b, true, 0)
}

func BenchmarkCreateFilePhotoSubscriberSharedBlobPhotoCatalog(b *testing.B) {
	benchmarkCreateFilePhotoSubscriber(b, true, 100000)
}

func benchmarkCreateFilePhotoSubscriber(b *testing.B, sharedBlob bool, photos int) {
	b.Helper()
	hash := func(i int) string {
		if sharedBlob {
			i = 1
		}
		return fmt.Sprintf("%064x", i)
	}
	for _, existing := range []int{0, 100000} {
		b.Run(fmt.Sprintf("existing=%d", existing), func(b *testing.B) {
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
						hash(i+1), 4, "application/octet-stream", "/synthetic/"+name, "",
						ingestFileOptions{observeMembership: true, exact: sharedBlob})
					if err != nil {
						return err
					}
				}
				for i := range photos {
					name := fmt.Sprintf("photo-%06d.jpg", i)
					_, _, _, err := s.ingestFileTx(ctx, tx, run, s.RootID(), name,
						fmt.Sprintf("%064x", i+3), 4, "image/jpeg", "/synthetic/"+name, "",
						ingestFileOptions{observeMembership: true, exact: true})
					if err != nil {
						return err
					}
				}
				return nil
			}))
			if sharedBlob {
				var versions int
				require.NoError(b, s.db.QueryRowContext(ctx, `SELECT count(*) FROM content_versions WHERE blob_hash=?`, hash(1)).Scan(&versions))
				require.Equal(b, existing, versions)
				// Keep the photo catalog nonempty without sharing the ordinary files' blob.
				_, err = s.CreateFile(ctx, s.RootID(), "unrelated.jpg", fmt.Sprintf("%064x", 2), 4, "image/jpeg")
				require.NoError(b, err)
				var photoFiles int
				require.NoError(b, s.db.QueryRowContext(ctx, `SELECT count(*) FROM photo_files`).Scan(&photoFiles))
				require.Equal(b, photos+1, photoFiles)
			}
			next := 0
			for _, audience := range []string{"none", "locked", "hidden"} {
				b.Run("subscriber="+audience, func(b *testing.B) {
					ctx, cancel, installed := setupPhotoBenchmarkCapture(b, s, audience)
					b.Cleanup(cancel)
					// Warm the measured write path in every case.
					_, err = s.CreateFile(ctx, s.RootID(), "warmup-"+audience+".bin", hash(0), 4, "application/octet-stream")
					require.NoError(b, err)
					require.NoError(b, s.Checkpoint(ctx))
					b.ReportAllocs()
					for b.Loop() {
						_, err := s.CreateFile(ctx, s.RootID(), fmt.Sprintf("file-%06d.bin", next),
							hash(existing+next+1), 4, "application/octet-stream")
						require.NoError(b, err)
						next++
					}
					b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N), "ns/file")
					b.ReportMetric(float64(installed.Microseconds())/1000, "capture-install-ms")
				})
			}
		})
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
