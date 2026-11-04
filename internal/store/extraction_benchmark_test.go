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
					s, err := Open(b.Context(), filepath.Join(dir, "docbank.db"))
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
					_, err := tx.ExecContext(ctx, `WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM seq WHERE i<$1)
						INSERT INTO nodes (id,parent_id,name,kind,current_version_id,revision,created_at,modified_at)
						SELECT $2+i,parent_id,printf('seed-%d.txt',i),kind,
						printf('00000000-0000-4000-8000-%012x',i),1,created_at,modified_at
						FROM seq CROSS JOIN nodes WHERE id=$2`, versions-1, first.ID)
					if err != nil {
						return err
					}
					_, err = tx.ExecContext(ctx, `WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM seq WHERE i<$1)
						INSERT INTO content_versions (version_id,node_id,blob_hash,size,mime_type,recorded_at,node_revision,
						introduced_operation_id,transition_kind,source_version_id)
						SELECT printf('00000000-0000-4000-8000-%012x',i),$2+i,blob_hash,size,mime_type,recorded_at,1,
						printf('00000000-0000-4000-9000-%012x',i),'content_create',NULL
						FROM seq CROSS JOIN content_versions WHERE version_id=$3`, versions-1, first.ID, first.CurrentVersionID)
					return err
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
