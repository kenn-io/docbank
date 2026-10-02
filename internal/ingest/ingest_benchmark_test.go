package ingest

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

// BenchmarkAddPaths measures net-new imports, including durable blob writes
// and metadata commits. Each iteration owns a fresh vault; source generation,
// vault startup, and shutdown are excluded from the measured interval.
func BenchmarkAddPaths(b *testing.B) {
	for _, size := range []int{1024, 8192, 1 << 20} {
		b.Run(fmt.Sprintf("bytes=%d", size), func(b *testing.B) {
			const files = 128
			source := b.TempDir()
			random := rand.NewChaCha8([32]byte{1})
			content := make([]byte, size)
			for i := range files {
				_, err := random.Read(content)
				require.NoError(b, err)
				require.NoError(b, os.WriteFile(filepath.Join(source,
					fmt.Sprintf("file-%04d.bin", i)), content, 0o600))
			}
			b.SetBytes(int64(files * size))
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				vault := b.TempDir()
				s, err := store.Open(filepath.Join(vault, "docbank.db"))
				require.NoError(b, err)
				blobs, err := blob.NewWithOptions(store.NewPackCatalog(s),
					filepath.Join(vault, "blobs"), blob.ManagedOptions())
				require.NoError(b, err)
				ing := &Ingester{Store: s, Blobs: blobs}
				b.StartTimer()
				report, err := ing.AddPaths(b.Context(), []string{source}, "/inbox")
				b.StopTimer()
				closeErr := blobs.Close()
				storeErr := s.Close()
				require.NoError(b, err)
				require.Empty(b, report.Failed)
				require.Equal(b, files, report.Added)
				require.NoError(b, closeErr)
				require.NoError(b, storeErr)
				b.StartTimer()
			}
		})
	}
}
