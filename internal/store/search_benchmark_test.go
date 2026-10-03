package store

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

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
