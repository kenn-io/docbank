package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

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
