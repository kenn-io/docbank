package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Hold the duplicate groups fixed while unrelated content grows. Measurements
// include population totals, paths, and collection previews for the page.
func BenchmarkDuplicates(b *testing.B) {
	for _, unrelated := range []int{0, 10000} {
		b.Run(fmt.Sprintf("unrelated=%d", unrelated), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			ctx := b.Context()
			hash := strings.Repeat("a1", 32)
			var first, second Node
			require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				for i := range 2 + unrelated {
					contentHash := hash
					if i >= 2 {
						contentHash = fmt.Sprintf("%064x", i)
					}
					node, _, err := s.createFileTx(ctx, tx, s.RootID(),
						fmt.Sprintf("file-%05d.bin", i), contentHash, 1024, "application/octet-stream")
					if err != nil {
						return err
					}
					switch i {
					case 0:
						first = node
					case 1:
						second = node
					}
				}
				return nil
			}))
			b.ReportAllocs()
			for b.Loop() {
				page, err := s.Duplicates(ctx, 50, 0)
				if err != nil || page.Total != 1 || page.TotalReferences != 2 || len(page.Items) != 1 {
					b.Fatalf("page=%+v err=%v", page, err)
				}
				group := page.Items[0]
				if group.ReferenceCount != 2 || len(group.References) != 2 ||
					group.RepresentativeNodeID != first.ID || group.ReferencesTruncated {
					b.Fatalf("group=%+v err=%v", group, err)
				}
				if group.References[0].Reference.Node.ID != first.ID ||
					group.References[1].Reference.Node.ID != second.ID ||
					group.References[0].Reference.Path != "/file-00000.bin" ||
					group.References[1].Reference.Path != "/file-00001.bin" {
					b.Fatal("duplicate group identities or paths changed")
				}
			}
		})
	}
}

// Vary page size, references per group, and ancestor depth.
// Fixture creation is excluded; reads include metadata, paths and collections.
func BenchmarkDuplicateReferencePages(b *testing.B) {
	for _, references := range []int{2, 16} {
		for _, groups := range []int{1, 50} {
			for _, depth := range []int{0, 16} {
				b.Run(fmt.Sprintf("references=%d/groups=%d/depth=%d", references, groups, depth), func(b *testing.B) {
					s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
					require.NoError(b, err)
					b.Cleanup(func() { require.NoError(b, s.Close()) })
					ctx := b.Context()
					prefix := strings.Repeat("/folder", depth)
					parent, err := s.MkdirAll(ctx, prefix)
					require.NoError(b, err)
					run, err := s.BeginIngest(ctx, "cli", "Synthetic duplicates")
					require.NoError(b, err)
					nodes := make([][]Node, groups)
					paths := make([][]string, groups)
					require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
						for group := range groups {
							nodes[group] = make([]Node, references)
							paths[group] = make([]string, references)
							for copy := range references {
								name := fmt.Sprintf("group-%03d-copy-%02d.bin", group, copy)
								receipt, _, _, err := s.ingestFileTx(ctx, tx, run, parent.ID, name,
									fmt.Sprintf("%064x", group+1), 1024, "application/octet-stream",
									"/synthetic/"+name, "", ingestFileOptions{})
								if err != nil {
									return err
								}
								nodes[group][copy] = receipt.Node
								paths[group][copy] = prefix + "/" + name
							}
						}
						return nil
					}))
					b.ReportAllocs()
					for b.Loop() {
						page, err := s.Duplicates(ctx, 50, 0)
						if err != nil || page.Total != groups || page.TotalReferences != groups*references ||
							len(page.Items) != groups {
							b.Fatalf("page=%+v err=%v", page, err)
						}
						for i, group := range page.Items {
							if group.ReferenceCount != references || len(group.References) != references || group.ReferencesTruncated {
								b.Fatalf("group=%+v", group)
							}
							for j, ref := range group.References {
								if ref.Reference.Node.ID != nodes[i][j].ID ||
									ref.Reference.Version.ID != nodes[i][j].CurrentVersionID ||
									ref.Reference.Path != paths[i][j] || ref.CollectionCount != 1 ||
									len(ref.Collections) != 1 || ref.Collections[0].ID != run.ID() ||
									ref.Collections[0].Label != nil || ref.CollectionsTruncated {
									b.Fatalf("reference %d/%d metadata, path or collections changed", i, j)
								}
							}
						}
					}
				})
			}
		}
	}
}

// Include empty previews and collection counts on both sides of the limit.
func BenchmarkDuplicateCollectionPreviews(b *testing.B) {
	for _, count := range []int{0, 1, 16, 17, 128} {
		b.Run(fmt.Sprintf("collections=%d", count), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			ctx := b.Context()
			hash := strings.Repeat("a1", 32)
			first, err := s.CreateFile(ctx, s.RootID(), "first.txt", hash, 12, "text/plain")
			require.NoError(b, err)
			second, err := s.CreateFile(ctx, s.RootID(), "second.txt", hash, 12, "text/plain")
			require.NoError(b, err)
			nodeIDs := []int64{first.ID, second.ID}
			ids := make([]string, 0, count)
			for range count {
				run, err := s.BeginIngest(ctx, "cli", "Synthetic collection")
				require.NoError(b, err)
				addCollectionMembership(b, s, run, first.ID, "/synthetic/first.txt", nil)
				addCollectionMembership(b, s, run, second.ID, "/synthetic/second.txt", nil)
				ids = append(ids, run.ID())
			}
			sort.Strings(ids)
			b.ReportAllocs()
			for b.Loop() {
				group, err := s.DuplicateGroupByHash(ctx, hash, 12)
				if err != nil || group.ReferenceCount != 2 || len(group.References) != 2 {
					b.Fatalf("group=%+v err=%v", group, err)
				}
				for i, ref := range group.References {
					if ref.Reference.Node.ID != nodeIDs[i] || ref.CollectionCount != count ||
						len(ref.Collections) != min(count, 16) || ref.Collections == nil ||
						ref.CollectionsTruncated != (count > 16) {
						b.Fatal("duplicate collection counts or truncation changed")
					}
					for j, collection := range ref.Collections {
						if collection.ID != ids[j] || collection.Label != nil {
							b.Fatal("duplicate collection identities or labels changed")
						}
					}
				}
			}
		})
	}
}
