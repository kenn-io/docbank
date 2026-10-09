package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Include the complete final validity check and path refresh for a result page.
func BenchmarkSearchRevalidation(b *testing.B) {
	for _, depth := range []int{0, 16} {
		b.Run(fmt.Sprintf("depth=%d", depth), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			ctx := b.Context()
			prefix := strings.Repeat("/folder", depth)
			parent, err := s.MkdirAll(ctx, prefix)
			require.NoError(b, err)
			candidates := make([]SearchCandidateIdentity, 1000)
			paths := make([]string, len(candidates))
			require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				for i := range candidates {
					name := fmt.Sprintf("report-%04d.txt", i)
					node, _, err := s.createFileTx(ctx, tx, parent.ID, name,
						fmt.Sprintf("%064x", i+1), 8, "text/plain")
					if err != nil {
						return err
					}
					candidates[i] = SearchCandidateIdentity{NodeID: node.ID, NodeRevision: node.Revision,
						ContentVersionID: node.CurrentVersionID, Evidence: []SearchEvidenceIdentity{{Kind: "node_name"}}}
					paths[i] = prefix + "/" + name
				}
				return nil
			}))
			for _, count := range []int{1, 50, 1000} {
				b.Run(fmt.Sprintf("candidates=%d", count), func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						result, err := s.RevalidateSearchCandidates(ctx, candidates[:count], SearchOptions{}, "", "")
						if err != nil || len(result.Candidates) != count {
							b.Fatalf("candidates=%d err=%v", len(result.Candidates), err)
						}
						for i, candidate := range result.Candidates {
							if candidate.NodeID != candidates[i].NodeID || candidate.Path != paths[i] {
								b.Fatalf("candidate %d: node=%d path=%q", i, candidate.NodeID, candidate.Path)
							}
						}
					}
				})
			}
		})
	}
}

// BenchmarkNameSearch measures returning 50 ranked results as the number of
// matching filenames grows. Catalog creation is excluded from the timer.
func BenchmarkNameSearch(b *testing.B) {
	for _, files := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("matches=%d", files), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			for i := range files {
				_, err := s.CreateFile(b.Context(), s.RootID(), fmt.Sprintf("report-%06d.bin", i),
					fmt.Sprintf("%064x", i+1), 1024, "application/octet-stream")
				require.NoError(b, err)
			}
			b.Run("page", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					hits, truncated, err := s.SearchPage(b.Context(), "report", 50)
					require.NoError(b, err)
					require.Len(b, hits, 50)
					require.True(b, truncated)
				}
			})
			b.Run("explained", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					hits, truncated, err := s.SearchExplainedLexicalCandidates(b.Context(), "report", 50, SearchOptions{}, false)
					require.NoError(b, err)
					require.Len(b, hits, 50)
					require.True(b, truncated)
				}
			})
		})
	}
}
