package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkDocumentCatalogPage includes page contents and both continuation
// checks. Fixture creation is excluded from timing.
func BenchmarkDocumentCatalogPage(b *testing.B) {
	for _, files := range []int{100, 10000} {
		b.Run(fmt.Sprintf("files=%d", files), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			ctx := b.Context()
			var middle DocumentCatalogPosition
			require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				for i := range files {
					name := fmt.Sprintf("file-%05d.bin", i)
					node, _, err := s.createFileTx(ctx, tx, s.RootID(), name,
						fmt.Sprintf("%064x", i+1), int64(i+1), "application/octet-stream")
					if err != nil {
						return err
					}
					if i == files/2-1 {
						middle = DocumentCatalogPosition{Path: "/" + name, Value: "/" + name, NodeID: node.ID}
					}
				}
				return nil
			}))
			for _, tc := range []struct {
				name     string
				boundary *DocumentCatalogPosition
				start    int
			}{
				{"first", nil, 0},
				{"middle", &middle, files / 2},
			} {
				b.Run(tc.name, func(b *testing.B) {
					query := DocumentCatalogQuery{PageSize: 50}
					firstPath := fmt.Sprintf("/file-%05d.bin", tc.start)
					lastPath := fmt.Sprintf("/file-%05d.bin", tc.start+49)
					b.ReportAllocs()
					for b.Loop() {
						page, err := s.ListDocuments(ctx, query, tc.boundary, DocumentCatalogTraversalNext)
						require.NoError(b, err)
						require.Len(b, page.Items, 50)
						require.Equal(b, firstPath, page.Items[0].Path)
						require.Equal(b, lastPath, page.Items[49].Path)
						require.Equal(b, tc.start > 0, page.HasPrevious)
						require.Equal(b, tc.start+50 < files, page.HasNext)
					}
				})
			}
		})
	}
}
