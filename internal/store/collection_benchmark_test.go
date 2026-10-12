package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkCollectionsPage keeps membership and page size fixed while the
// number of collections grows. Catalog creation is excluded from timing.
func BenchmarkCollectionsPage(b *testing.B) {
	for _, collections := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("collections=%d", collections), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			ctx := b.Context()
			ids := make([]string, collections)
			require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				nodes := make([]Node, 100)
				for i := range nodes {
					var err error
					nodes[i], _, err = s.createFileTx(ctx, tx, s.RootID(),
						fmt.Sprintf("file-%03d.bin", i), fmt.Sprintf("%064x", i+1),
						8, "application/octet-stream")
					if err != nil {
						return err
					}
				}
				for i := range ids {
					run, err := s.BeginIngest(ctx, "cli", "Synthetic import")
					if err != nil {
						return err
					}
					run.record.StartedAt = "2026-01-01T00:00:00.000000000Z"
					ids[i] = run.ID()
					if _, err := ensureIngestRunTx(ctx, tx, run); err != nil {
						return err
					}
					for _, node := range nodes {
						fact := metadataProvenance{
							Type: metadataProvenanceType, NodeID: node.ID, IngestID: run.ID(),
							OriginalPath: "/synthetic/" + node.Name,
						}
						identity, err := provenanceIdentity(fact)
						if err != nil {
							return err
						}
						if _, err := tx.ExecContext(ctx, `INSERT INTO provenance(
							identity,node_id,ingest_id,original_path) VALUES(?,?,?,?)`,
							identity, node.ID, run.ID(), fact.OriginalPath); err != nil {
							return err
						}
					}
				}
				return nil
			}))
			slices.Sort(ids)
			for _, offset := range slices.Compact([]int{0, collections - 10}) {
				b.Run(fmt.Sprintf("offset=%d", offset), func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						page, total, err := s.Collections(ctx, 10, offset)
						require.NoError(b, err)
						require.Equal(b, collections, total)
						require.Len(b, page, 10)
						for j, collection := range page {
							require.Equal(b, ids[offset+j], collection.ID)
							require.Equal(b, int64(100), collection.FileCount)
							require.Equal(b, int64(800), collection.TotalBytes)
						}
					}
				})
			}
		})
	}
}

// Keep the selected collection fixed while the rest of the vault grows.
func BenchmarkCollectionReads(b *testing.B) {
	for _, unrelated := range []int{0, 1000, 10000} {
		b.Run(fmt.Sprintf("unrelated=%d", unrelated), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			ctx := b.Context()
			target, err := s.BeginIngest(ctx, "cli", "Selected synthetic import")
			require.NoError(b, err)
			other, err := s.BeginIngest(ctx, "cli", "Unrelated synthetic import")
			require.NoError(b, err)
			require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				for i := range 100 + unrelated {
					run := target
					if i >= 100 {
						run = other
					}
					name := fmt.Sprintf("file-%05d.txt", i)
					_, _, _, err := s.ingestFileTx(ctx, tx, run, s.RootID(),
						name, fmt.Sprintf("%064x", i+1), 8, "text/plain", "/synthetic/"+name, "",
						ingestFileOptions{})
					if err != nil {
						return err
					}
				}
				return nil
			}))
			b.Run("detail", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					collection, err := s.CollectionByID(ctx, target.ID())
					if err != nil || collection.FileCount != 100 || collection.TotalBytes != 800 {
						b.Fatalf("collection = %+v, err = %v", collection, err)
					}
				}
			})
			b.Run("members", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					page, err := s.CollectionMembers(ctx, target.ID(), 50, 0)
					if err != nil || page.Total != 100 || len(page.Items) != 50 {
						b.Fatalf("total = %d, members = %d, err = %v", page.Total, len(page.Items), err)
					}
				}
			})
			for _, selection := range []struct {
				name  string
				run   IngestRun
				files int64
			}{{"quality", target, 100}, {"quality_other", other, int64(unrelated)}} {
				if selection.files == 0 {
					continue
				}
				b.Run(selection.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						report, err := s.CollectionQuality(ctx, selection.run.ID(), CoverageSelection{}, nil)
						if err != nil || report.Collection.FileCount != selection.files ||
							report.Collection.TotalBytes != 8*selection.files || report.DuplicateDocuments != 0 {
							b.Fatalf("quality = %+v, err = %v", report, err)
						}
					}
				})
			}
		})
	}
}

// Hold collection size fixed while varying page size and ancestor depth.
func BenchmarkCollectionMemberPaths(b *testing.B) {
	for _, depth := range []int{0, 16} {
		b.Run(fmt.Sprintf("depth=%d", depth), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			ctx := b.Context()
			prefix := strings.Repeat("/folder", depth)
			parent, err := s.MkdirAll(ctx, prefix)
			require.NoError(b, err)
			run, err := s.BeginIngest(ctx, "cli", "Synthetic nested import")
			require.NoError(b, err)
			paths := make([]string, 1000)
			require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				for i := range paths {
					name := fmt.Sprintf("file-%04d.txt", i)
					paths[i] = prefix + "/" + name
					_, _, _, err := s.ingestFileTx(ctx, tx, run, parent.ID,
						name, fmt.Sprintf("%064x", i+1), 8, "text/plain", "/synthetic/"+name, "",
						ingestFileOptions{})
					if err != nil {
						return err
					}
				}
				return nil
			}))
			for _, limit := range []int{1, 100, 1000} {
				b.Run(fmt.Sprintf("limit=%d", limit), func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						page, err := s.CollectionMembers(ctx, run.ID(), limit, 0)
						if err != nil || page.Total != len(paths) || len(page.Items) != limit {
							b.Fatalf("total=%d items=%d err=%v", page.Total, len(page.Items), err)
						}
						for i, item := range page.Items {
							if item.Path != paths[i] {
								b.Fatalf("path=%q want=%q", item.Path, paths[i])
							}
						}
					}
				})
			}
		})
	}
}
