package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

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

// BenchmarkTaggedNodes measures a bounded page as the tag's membership grows.
// Catalog creation and assignments are excluded from timing.
func BenchmarkTaggedNodes(b *testing.B) {
	for _, files := range []int{100, 10000} {
		b.Run(fmt.Sprintf("files=%d", files), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			tag, err := s.CreateTag(b.Context(), "records")
			require.NoError(b, err)
			require.NoError(b, s.withStorageTx(b.Context(), func(tx *sql.Tx) error {
				for i := range files {
					node, _, err := s.createFileTx(b.Context(), tx, s.RootID(),
						fmt.Sprintf("file-%06d.bin", i), fmt.Sprintf("%064x", i+1),
						128, "application/octet-stream")
					if err != nil {
						return err
					}
					if _, err := tx.ExecContext(b.Context(),
						`INSERT INTO node_tags(node_id,tag_id) VALUES(?,?)`, node.ID, tag.ID,
					); err != nil {
						return err
					}
				}
				return nil
			}))
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
