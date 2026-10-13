package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkNodeTags measures a node's tag pages as its assignments grow.
// Each tag has 100 members; synthetic catalog creation is outside the timer.
func BenchmarkNodeTags(b *testing.B) {
	for _, definitions := range []int{1, 100, 1000} {
		b.Run(fmt.Sprintf("tags=%d", definitions), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			ctx := b.Context()
			var selected int64
			require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				nodes := make([]int64, 100)
				for i := range nodes {
					node, _, err := s.createFileTx(ctx, tx, s.RootID(),
						fmt.Sprintf("file-%04d.bin", i), fmt.Sprintf("%064x", i+1),
						128, "application/octet-stream")
					if err != nil {
						return err
					}
					nodes[i] = node.ID
				}
				selected = nodes[0]
				for i := range definitions {
					// Permute IDs so membership-index order differs from name order.
					id := fmt.Sprintf("%08x-1111-4111-8111-111111111111", (i*7919)%definitions+1)
					if _, err := tx.ExecContext(ctx, `INSERT INTO tags(id,name) VALUES(?,?)`,
						id, fmt.Sprintf("tag-%04d", i)); err != nil {
						return err
					}
					for _, nodeID := range nodes {
						if _, err := tx.ExecContext(ctx,
							`INSERT INTO node_tags(node_id,tag_id) VALUES(?,?)`, nodeID, id,
						); err != nil {
							return err
						}
					}
				}
				return nil
			}))
			for _, limit := range slices.Compact([]int{min(definitions, 50), min(definitions, 100), definitions}) {
				for _, offset := range slices.Compact([]int{0, definitions - limit}) {
					b.Run(fmt.Sprintf("limit=%d/offset=%d", limit, offset), func(b *testing.B) {
						first, last := fmt.Sprintf("tag-%04d", offset), fmt.Sprintf("tag-%04d", offset+limit-1)
						b.ReportAllocs()
						for b.Loop() {
							page, total, err := s.NodeTags(ctx, selected, limit, offset)
							if err != nil || total != definitions || len(page) != limit {
								b.Fatalf("tags=%d total=%d err=%v", len(page), total, err)
							}
							if page[0].Name != first || page[len(page)-1].Name != last || page[0].AssignmentCount != 100 {
								b.Fatal("unexpected node tag page")
							}
						}
					})
				}
			}
		})
	}
}

// BenchmarkTags measures listing definitions as assignments outside the page grow.
func BenchmarkTags(b *testing.B) {
	for _, definitions := range []int{100, 1000} {
		b.Run(fmt.Sprintf("tags=%d", definitions), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			tags := make([]Tag, definitions)
			for i := range tags {
				tags[i], err = s.CreateTag(b.Context(), fmt.Sprintf("tag-%04d", i))
				require.NoError(b, err)
			}
			require.NoError(b, s.withStorageTx(b.Context(), func(tx *sql.Tx) error {
				nodes := make([]int64, 100)
				for i := range nodes {
					node, _, err := s.createFileTx(b.Context(), tx, s.RootID(),
						fmt.Sprintf("file-%04d.bin", i), fmt.Sprintf("%064x", i+1),
						128, "application/octet-stream")
					if err != nil {
						return err
					}
					nodes[i] = node.ID
				}
				for _, tag := range tags {
					for _, nodeID := range nodes {
						if _, err := tx.ExecContext(b.Context(),
							`INSERT INTO node_tags(node_id,tag_id) VALUES(?,?)`, nodeID, tag.ID,
						); err != nil {
							return err
						}
					}
				}
				return nil
			}))
			for _, offset := range []int{0, definitions - 50} {
				b.Run(fmt.Sprintf("offset=%d", offset), func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						page, total, err := s.Tags(b.Context(), 50, offset)
						require.NoError(b, err)
						require.Equal(b, definitions, total)
						require.Len(b, page, 50)
						require.Equal(b, fmt.Sprintf("tag-%04d", offset), page[0].Name)
						require.Equal(b, fmt.Sprintf("tag-%04d", offset+49), page[49].Name)
						require.Equal(b, 100, page[0].AssignmentCount)
					}
				})
			}
		})
	}
}

// BenchmarkTaggedNodes measures tag lookups, filtered searches, and bounded node
// pages as the catalog grows. Creation and assignments are excluded from timing.
func BenchmarkTaggedNodes(b *testing.B) {
	for _, files := range []int{0, 100, 10000} {
		b.Run(fmt.Sprintf("files=%d", files), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			tag, err := s.CreateTag(b.Context(), "records")
			require.NoError(b, err)
			var selected Node
			require.NoError(b, s.withStorageTx(b.Context(), func(tx *sql.Tx) error {
				for i := range files {
					node, _, err := s.createFileTx(b.Context(), tx, s.RootID(),
						fmt.Sprintf("file-%06d.bin", i), fmt.Sprintf("%064x", i+1),
						128, "application/octet-stream")
					if err != nil {
						return err
					}
					if i == 0 {
						selected = node
					}
					if _, err := tx.ExecContext(b.Context(),
						`INSERT INTO node_tags(node_id,tag_id) VALUES(?,?)`, node.ID, tag.ID,
					); err != nil {
						return err
					}
				}
				return nil
			}))
			for _, mode := range []string{"lookup_id", "lookup_name", "assign_noop"} {
				if mode == "assign_noop" && files == 0 {
					continue
				}
				b.Run(mode, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						var got Tag
						var err error
						switch mode {
						case "lookup_id":
							got, err = s.TagByID(b.Context(), tag.ID)
						case "lookup_name":
							got, err = s.TagByName(b.Context(), tag.Name)
						case "assign_noop":
							var change TagAssignmentChange
							change, err = s.AssignTag(b.Context(), tag.ID, selected.ID, selected.Revision)
							if change.Changed {
								b.Fatal("changed existing assignment")
							}
							got = change.Tag
						}
						if err != nil || got.ID != tag.ID || got.AssignmentCount != files {
							b.Fatalf("tag=%+v err=%v", got, err)
						}
					}
				})
			}
			opts := SearchOptions{TagID: tag.ID}
			b.Run("search_validate", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					got, err := s.NormalizeSearchOptions(b.Context(), opts)
					if err != nil || got.TagID != tag.ID {
						b.Fatalf("options=%+v err=%v", got, err)
					}
				}
			})
			sparseTag, err := s.CreateTag(b.Context(), "one-record")
			require.NoError(b, err)
			if files != 0 {
				_, err = s.db.ExecContext(b.Context(),
					`INSERT INTO node_tags(node_id,tag_id) VALUES(?,?)`, selected.ID, sparseTag.ID)
				require.NoError(b, err)
			}
			for _, scope := range []struct {
				name, tagID string
				members     int
			}{
				{"all", tag.ID, files},
				{"one", sparseTag.ID, min(files, 1)},
			} {
				for _, query := range []string{"file-000000", "file", ""} {
					want := scope.members
					if query == "file-000000" {
						want = min(want, 1)
					}
					b.Run(fmt.Sprintf("search/members=%s/query=%s", scope.name, query), func(b *testing.B) {
						b.ReportAllocs()
						for b.Loop() {
							hits, truncated, err := s.SearchPageWithOptions(b.Context(), query, 50,
								SearchOptions{TagID: scope.tagID})
							if err != nil || truncated != (want > 50) || len(hits) != min(want, 50) {
								b.Fatalf("hits=%d truncated=%t err=%v", len(hits), truncated, err)
							}
							if want == 1 && (hits[0].Node.ID != selected.ID || hits[0].Path != "/file-000000.bin") {
								b.Fatalf("unexpected hit: %+v", hits[0])
							}
						}
					})
				}
			}

			if files == 0 {
				return
			}
			for _, liveOnly := range []bool{false, true} {
				for _, offset := range []int{0, files - 50} {
					b.Run(fmt.Sprintf("live=%t/offset=%d", liveOnly, offset), func(b *testing.B) {
						b.ReportAllocs()
						for b.Loop() {
							nodes, total, omitted, err := s.taggedNodes(b.Context(), tag.ID, 50, offset, liveOnly)
							require.NoError(b, err)
							require.Equal(b, files, total)
							require.Zero(b, omitted)
							require.Len(b, nodes, 50)
							require.Equal(b, fmt.Sprintf("/file-%06d.bin", offset), nodes[0].Path)
							require.Equal(b, fmt.Sprintf("/file-%06d.bin", offset+49), nodes[49].Path)
						}
					})
				}
			}
		})
	}
}
