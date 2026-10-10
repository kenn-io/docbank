package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
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

func BenchmarkResolveDocumentSummaries(b *testing.B) {
	for _, corpus := range []struct{ files, depth int }{
		{100, 0}, {10000, 0}, {100, 8}, {10000, 8},
	} {
		b.Run(fmt.Sprintf("files=%d/depth=%d", corpus.files, corpus.depth), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			ctx := b.Context()
			prefix := strings.Repeat("/dir", corpus.depth)
			parent, err := s.MkdirAll(ctx, prefix)
			require.NoError(b, err)
			identities := make([]DocumentCatalogIdentity, 0, 50)
			require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				for i := range corpus.files {
					name := fmt.Sprintf("file-%05d.bin", i)
					node, _, err := s.createFileTx(ctx, tx, parent.ID, name,
						fmt.Sprintf("%064x", i+1), int64(i+1), "application/octet-stream")
					if err != nil {
						return err
					}
					if i >= corpus.files-50 {
						identities = append(identities, DocumentCatalogIdentity{
							NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Path: prefix + "/" + name,
						})
					}
				}
				return nil
			}))
			for _, count := range []int{1, 50} {
				b.Run(fmt.Sprintf("selected=%d", count), func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						items, err := s.ResolveDocumentSummaries(ctx, identities[:count])
						require.NoError(b, err)
						require.Len(b, items, count)
						require.Equal(b, identities[0].Path, items[0].Path)
						require.Equal(b, identities[count-1].Path, items[count-1].Path)
					}
				})
			}
		})
	}
}
