package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

// BenchmarkMissingSourceMetadataTargets includes caught-up worker scans as
// well as pending pages. Synthetic catalog creation is outside the timer.
func BenchmarkMissingSourceMetadataTargets(b *testing.B) {
	for _, corpus := range []struct{ blobs, copies int }{{1, 1}, {100, 1}, {100, 100}, {1000, 1}} {
		b.Run(fmt.Sprintf("blobs=%d/copies=%d", corpus.blobs, corpus.copies), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			ctx := b.Context()
			require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				for blob := range corpus.blobs {
					for copy := range corpus.copies {
						_, _, err := s.createFileTx(ctx, tx, s.RootID(),
							fmt.Sprintf("source-%04d-copy-%03d.pdf", blob, copy),
							fmt.Sprintf("%064x", blob+1), int64(blob+1), "application/pdf")
						if err != nil {
							return err
						}
					}
				}
				return nil
			}))
			canonical, _, err := document.MarshalSourceMetadataV1(document.SourceMetadataV1{
				ContractVersion: document.SourceMetadataContractV1,
			})
			require.NoError(b, err)
			fingerprint := fmt.Sprintf("%064x", 10001)
			published := 0
			for _, completed := range slices.Compact([]int{0, corpus.blobs - 1, corpus.blobs}) {
				for published < completed {
					_, err := s.PublishSourceMetadata(ctx, fmt.Sprintf("%064x", published+1), fingerprint, canonical)
					require.NoError(b, err)
					published++
				}
				b.Run(fmt.Sprintf("completed=%d", completed), func(b *testing.B) {
					count := min(10, corpus.blobs-completed)
					b.ReportAllocs()
					for b.Loop() {
						targets, err := s.MissingSourceMetadataTargetsAfter(ctx, fingerprint, "", 10)
						if err != nil || len(targets) != count {
							b.Fatalf("targets=%d err=%v", len(targets), err)
						}
						for i, target := range targets {
							if target.SourceSHA256 != fmt.Sprintf("%064x", completed+i+1) || target.Size != int64(completed+i+1) {
								b.Fatal("unexpected source metadata target")
							}
						}
					}
				})
			}
		})
	}
}

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
