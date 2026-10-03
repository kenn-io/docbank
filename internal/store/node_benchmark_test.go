package store

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkDirectoryChildrenPage measures listing a bounded page as its
// directory grows. Creating the synthetic catalog is outside the timer.
func BenchmarkDirectoryChildrenPage(b *testing.B) {
	for _, children := range []int{100, 10000} {
		b.Run(fmt.Sprintf("children=%d", children), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			for i := range children {
				_, err := s.CreateFile(b.Context(), s.RootID(), fmt.Sprintf("file-%06d.bin", i),
					fmt.Sprintf("%064x", i+1), 8192, "application/octet-stream")
				require.NoError(b, err)
			}
			for _, offset := range slices.Compact([]int{0, children - 100}) {
				b.Run(fmt.Sprintf("offset=%d", offset), func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						page, err := s.DirectoryChildrenPage(b.Context(), s.RootID(), 100, offset)
						require.NoError(b, err)
						require.Equal(b, children, page.Total)
						require.Len(b, page.Children, 100)
					}
				})
			}
		})
	}
}
