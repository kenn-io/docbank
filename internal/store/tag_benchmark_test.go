package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

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
