package store

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkBatchMove measures complete transactions in a 1,000-file catalog.
// Fixture creation is excluded; changed batches alternate between two names.
func BenchmarkBatchMove(b *testing.B) {
	for _, depth := range []int{1, 10} {
		for _, count := range []int{1, 100, 1000} {
			for _, noop := range []bool{false, true} {
				b.Run(fmt.Sprintf("depth=%d/moves=%d/noop=%t", depth, count, noop), func(b *testing.B) {
					s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
					require.NoError(b, err)
					b.Cleanup(func() { require.NoError(b, s.Close()) })
					ctx := b.Context()
					parentID, parentPath := s.RootID(), ""
					for level := range depth {
						name := fmt.Sprintf("level-%02d", level)
						node, err := s.Mkdir(ctx, parentID, name)
						require.NoError(b, err)
						parentID, parentPath = node.ID, parentPath+"/"+name
					}
					requests := make([]BatchMoveRequest, count)
					ids := make([]int64, count)
					for i := range 1000 {
						name := fmt.Sprintf("file-%04d.txt", i)
						node, err := s.CreateFile(ctx, parentID, name,
							fmt.Sprintf("%064x", i+1), 128, "text/plain")
						require.NoError(b, err)
						if i < count {
							ids[i] = node.ID
							requests[i] = BatchMoveRequest{
								SourcePath:      parentPath + "/" + name,
								DestinationPath: parentPath + "/renamed-" + name,
							}
							if noop {
								requests[i].DestinationPath = requests[i].SourcePath
							}
						}
					}
					b.ReportAllocs()
					for b.Loop() {
						results, err := s.BatchMove(ctx, requests)
						if err != nil || len(results) != count {
							b.Fatalf("moves=%d err=%v", len(results), err)
						}
						for i, result := range results {
							if result.Node.ID != ids[i] || result.FromPath != requests[i].SourcePath ||
								result.Path != requests[i].DestinationPath {
								b.Fatal("unexpected move receipt")
							}
							requests[i].SourcePath, requests[i].DestinationPath =
								requests[i].DestinationPath, requests[i].SourcePath
						}
					}
				})
			}
		}
	}
}
