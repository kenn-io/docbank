package ingest

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

// BenchmarkFolderWatchIdleTick holds file count constant to isolate the cost of
// unchanged bytes from the per-file observation cost on periodic scans.
func BenchmarkFolderWatchIdleTick(b *testing.B) {
	for _, size := range []int{64 << 10, 1 << 20, 16 << 20} {
		b.Run(fmt.Sprintf("bytes=%d", 64*size), func(b *testing.B) {
			source := b.TempDir()
			content := make([]byte, size)
			for i := range 64 {
				require.NoError(b, os.WriteFile(filepath.Join(source, fmt.Sprintf("file-%04d.bin", i)), content, 0o600))
			}
			processed := 0
			scanner, err := NewFolderScanner(config.WatchConfig{
				Name: "benchmark", Source: source, Destination: "/archive",
				SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Second),
			}, func(ctx context.Context, file FolderFile) error {
				processed++
				_, err := file.Digest(ctx)
				return err
			}, func(_ context.Context, ref string, err error) error {
				b.Fatalf("rejected %s: %v", ref, err)
				return err
			})
			require.NoError(b, err)
			now := time.Now()
			scanner.watcher.now = func() time.Time { return now }
			root, err := scanner.openRoot()
			require.NoError(b, err)
			defer func() { require.NoError(b, root.Close()) }()
			require.NoError(b, scanner.watcher.scan(b.Context(), root))
			now = now.Add(time.Second)
			require.NoError(b, scanner.watcher.scan(b.Context(), root))
			require.Equal(b, 64, processed)
			b.ReportAllocs()
			for b.Loop() {
				complete, err := scanner.watcher.scanWithHints(b.Context(), root, watchScanHints{Reconcile: true})
				require.NoError(b, err)
				require.True(b, complete)
				require.Equal(b, 64, processed)
			}
		})
	}
}

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
				s, err := store.Open(b.Context(), filepath.Join(vault, "docbank.db"))
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
				s, err := store.Open(b.Context(), filepath.Join(vault, "docbank.db"))
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
