package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkIngestMetadata includes one metadata transaction per file, without
// blob I/O. BenchmarkAddPaths in internal/ingest measures durable blob writes too.
// Each iteration starts a fresh catalog; seeding and checkpointing are untimed.
func BenchmarkIngestMetadata(b *testing.B) {
	const batch = 128
	for _, mimeType := range []string{"application/octet-stream", "text/plain"} {
		for _, existing := range []int{0, 10000} {
			b.Run(fmt.Sprintf("%s/existing=%d", mimeType, existing), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					b.StopTimer()
					dir := b.TempDir()
					s, err := Open(filepath.Join(dir, "docbank.db"))
					require.NoError(b, err)
					require.NoError(b, s.withStorageTx(b.Context(), func(tx *sql.Tx) error {
						for i := range existing {
							_, _, err := s.createFileTx(b.Context(), tx, s.RootID(),
								fmt.Sprintf("seed-%05d.bin", i), fmt.Sprintf("%064x", i+1), 8192, "application/octet-stream")
							if err != nil {
								return err
							}
						}
						return nil
					}))
					run, err := s.BeginIngest(b.Context(), "cli", "Synthetic corpus")
					require.NoError(b, err)
					require.NoError(b, s.Checkpoint(b.Context()))
					b.StartTimer()
					for i := range batch {
						name := fmt.Sprintf("file-%04d", i)
						_, added, err := s.IngestFileWithMembership(b.Context(), run, s.RootID(),
							name, fmt.Sprintf("%064x", existing+i+1), 8192, mimeType, "/synthetic/"+name, "")
						require.NoError(b, err)
						require.True(b, added)
					}
					b.StopTimer()
					require.NoError(b, s.Close())
					require.NoError(b, os.RemoveAll(dir))
					b.StartTimer()
				}
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*batch), "ns/file")
			})
		}
	}
}

func BenchmarkRebuildTextExtractionState(b *testing.B) {
	for _, files := range []int{128, 1000} {
		b.Run(fmt.Sprintf("files=%d", files), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			for i := range files {
				_, err := s.CreateFile(b.Context(), s.RootID(), fmt.Sprintf("file-%04d.txt", i),
					fmt.Sprintf("%064x", i+1), 8192, "text/plain")
				require.NoError(b, err)
			}
			b.ReportAllocs()
			for b.Loop() {
				require.NoError(b, s.withStorageTx(b.Context(), func(tx *sql.Tx) error {
					return rebuildImportedTextExtractionStateTx(b.Context(), tx)
				}))
			}
			var queued, searchable int
			require.NoError(b, s.db.QueryRow(`SELECT count(*) FROM text_extraction_queue`).Scan(&queued))
			require.NoError(b, s.db.QueryRow(`SELECT count(*) FROM text_searchable_versions`).Scan(&searchable))
			require.Equal(b, files, queued)
			require.Equal(b, files, searchable)
		})
	}
}
