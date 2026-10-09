package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

// BenchmarkNodeSourceMetadataView includes the complete node-detail read,
// including its transaction, path, content version, and source metadata.
func BenchmarkNodeSourceMetadataView(b *testing.B) {
	for _, depth := range []int{1, 8, 32} {
		b.Run(fmt.Sprintf("depth=%d", depth), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			parentPath := strings.Repeat("/folder", depth-1)
			parent, err := s.MkdirAll(b.Context(), parentPath)
			require.NoError(b, err)
			node, err := s.CreateFile(b.Context(), parent.ID, "file.pdf",
				fmt.Sprintf("%064x", 1), 8192, "application/pdf")
			require.NoError(b, err)
			canonical, _, err := document.MarshalSourceMetadataV1(document.SourceMetadataV1{
				ContractVersion: document.SourceMetadataContractV1,
			})
			require.NoError(b, err)
			_, err = s.PublishSourceMetadata(b.Context(), node.BlobHash, fmt.Sprintf("%064x", 2), canonical)
			require.NoError(b, err)
			path := parentPath + "/file.pdf"
			for _, byPath := range []bool{false, true} {
				b.Run(fmt.Sprintf("by_path=%t", byPath), func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						var view NodeSourceMetadataView
						var err error
						if byPath {
							view, err = s.NodeSourceMetadataViewByPath(b.Context(), path)
						} else {
							view, err = s.NodeSourceMetadataViewByID(b.Context(), node.ID)
						}
						require.NoError(b, err)
						require.Equal(b, node, view.Node)
						require.Equal(b, path, view.Path)
						require.NotNil(b, view.SourceMetadata)
						require.Equal(b, path, view.SourceMetadata.Attachment.Path)
					}
				})
			}
		})
	}
}
