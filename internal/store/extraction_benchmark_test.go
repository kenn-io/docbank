package store

import (
	"context"
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
		type ingestCase struct {
			existing    int
			shared      bool
			destination int
		}
		cases := []ingestCase{{0, false, 0}, {1000, false, 0}, {10000, false, 0}, {1000, true, 0}, {10000, true, 0}}
		if os.Getenv("DOCBANK_INGEST_SCALE") != "" {
			cases = append(cases, ingestCase{100000, false, 0}, ingestCase{100000, true, 0}, ingestCase{1025, true, 100000})
		}
		for _, tc := range cases {
			existing := tc.existing
			b.Run(fmt.Sprintf("%s/existing=%d/shared=%t/destination=%d", mimeType, existing, tc.shared, tc.destination), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					b.StopTimer()
					dir := b.TempDir()
					s, err := Open(b.Context(), filepath.Join(dir, "docbank.db"))
					require.NoError(b, err)
					require.NoError(b, s.withStorageTx(b.Context(), func(tx *sql.Tx) error {
						if tc.shared {
							return seedSharedIngestBenchmark(b.Context(), s, tx, existing, tc.destination, 8192, "application/octet-stream")
						}
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
						hash := fmt.Sprintf("%064x", existing+i+1)
						if tc.shared {
							hash = fakeHash("a1")
						}
						_, added, err := s.IngestFileWithMembership(b.Context(), run, s.RootID(),
							name, hash, 8192, mimeType, "/synthetic/"+name, "")
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
			s, err := Open(b.Context(), filepath.Join(b.TempDir(), "docbank.db"))
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
			require.NoError(b, s.db.QueryRowContext(b.Context(), `SELECT count(*) FROM text_extraction_queue`).Scan(&queued))
			require.NoError(b, s.db.QueryRowContext(b.Context(), `SELECT count(*) FROM text_searchable_versions`).Scan(&searchable))
			require.Equal(b, files, queued)
			require.Equal(b, files, searchable)
		})
	}
}

func BenchmarkSeedTextExtractionQueue(b *testing.B) {
	for _, settled := range []bool{true, false} {
		for _, versions := range []int{1_000, 10_000, 100_000} {
			b.Run(fmt.Sprintf("settled=%t/versions=%d", settled, versions), func(b *testing.B) {
				ctx := b.Context()
				s := newTestStore(b)
				first, err := s.CreateFile(ctx, s.RootID(), "first.txt", fakeHash("a1"), 20, "text/plain")
				require.NoError(b, err)
				require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
					return bulkBenchmarkFileVersionsTx(ctx, tx, first, first.ID, versions-1)
				}))
				require.NoError(b, s.SeedTextExtractionQueue(ctx, "plain-text", 1))
				if settled {
					require.NoError(b, s.RecordExtraction(ctx, ExtractionResult{
						BlobHash: first.BlobHash, Extractor: "plain-text", ExtractorVersion: 1,
						Status: ExtractionOK, Text: "synthetic text",
					}))
				}
				b.ReportAllocs()
				for b.Loop() {
					require.NoError(b, s.SeedTextExtractionQueue(ctx, "plain-text", 1))
				}
				var searchable, queued int
				require.NoError(b, s.db.QueryRowContext(ctx, `SELECT count(*) FROM text_searchable_versions`).Scan(&searchable))
				require.NoError(b, s.db.QueryRowContext(ctx, `SELECT count(*) FROM text_extraction_queue`).Scan(&queued))
				require.Equal(b, versions, searchable)
				if settled {
					require.Zero(b, queued)
				} else {
					require.Equal(b, 1, queued)
				}
			})
		}
	}
}

// seedSharedIngestBenchmark puts repeated content outside the destination and
// unrelated files inside it. Both document and photo imports use this shape.
func seedSharedIngestBenchmark(ctx context.Context, s *Store, tx *sql.Tx, copies, destination int, size int64, mediaType string) error {
	parent, err := s.mkdirTx(ctx, tx, s.RootID(), "elsewhere", nowRFC3339())
	if err != nil {
		return err
	}
	for i := range copies {
		if _, _, err := s.createFileTx(ctx, tx, parent.ID, fmt.Sprintf("copy-%05d.bin", i), fakeHash("a1"), size, mediaType); err != nil {
			return err
		}
	}
	for i := range destination {
		if _, _, err := s.createFileTx(ctx, tx, s.RootID(), fmt.Sprintf("seed-%05d.bin", i), fmt.Sprintf("%064x", i+1000000), size, mediaType); err != nil {
			return err
		}
	}
	return nil
}
