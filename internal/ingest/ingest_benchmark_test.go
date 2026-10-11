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
	for _, tc := range []struct {
		name         string
		size, files  int
		shareContent bool
	}{
		{"bytes=1024", 1024, 128, false},
		{"bytes=8192", 8192, 128, false},
		{"bytes=1048576", 1 << 20, 128, false},
		{"shared-content/files=100", 1024, 100, true},
		{"shared-content/files=1000", 1024, 1000, true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			source := b.TempDir()
			random := rand.NewChaCha8([32]byte{1})
			content := make([]byte, tc.size)
			for i := range tc.files {
				if i == 0 || !tc.shareContent {
					_, err := random.Read(content)
					require.NoError(b, err)
				}
				require.NoError(b, os.WriteFile(filepath.Join(source,
					fmt.Sprintf("file-%04d.bin", i)), content, 0o600))
			}
			b.SetBytes(int64(tc.files * tc.size))
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
				require.Equal(b, tc.files, report.Added)
				require.NoError(b, closeErr)
				require.NoError(b, storeErr)
				b.StartTimer()
			}
		})
	}
}

// BenchmarkAddLabeledDirectoryTree includes traversal and the final metadata
// transaction for a flat empty tree. Source creation and vault startup and
// shutdown are excluded. Empty trees exercise finalization without blob I/O.
func BenchmarkAddLabeledDirectoryTree(b *testing.B) {
	for _, directories := range []int{1000, 10000, 30000} {
		b.Run(fmt.Sprintf("directories=%d", directories), func(b *testing.B) {
			source := b.TempDir()
			for i := range directories {
				require.NoError(b, os.Mkdir(filepath.Join(source,
					fmt.Sprintf("dir-%05d", i)), 0o700))
			}
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
				report, err := ing.AddPathsWithOptions(b.Context(), []string{source}, "/inbox", Options{
					CollectionLabel: new("Synthetic tree"),
				})
				b.StopTimer()
				_, lookupErr := s.NodeByPath(b.Context(),
					"/inbox/"+filepath.Base(source)+fmt.Sprintf("/dir-%05d", directories-1))
				closeErr := blobs.Close()
				storeErr := s.Close()
				require.NoError(b, err)
				require.Empty(b, report.Failed)
				require.Empty(b, report.IngestID)
				require.Zero(b, report.Added)
				require.NoError(b, lookupErr)
				require.NoError(b, closeErr)
				require.NoError(b, storeErr)
				b.StartTimer()
			}
		})
	}
}
