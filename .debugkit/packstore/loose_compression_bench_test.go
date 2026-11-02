package packstore

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func BenchmarkLooseWriteRaw(b *testing.B) {
	benchmarkLooseWrite(b, false)
}

func BenchmarkLooseWriteCompressed(b *testing.B) {
	benchmarkLooseWrite(b, true)
}

func benchmarkLooseWrite(b *testing.B, compressed bool) {
	b.Helper()
	require := require.New(b)
	b.Helper()
	content := representativeNDJSON(4_000)
	layout, err := NewLayout(b.TempDir(), LayoutOptions{Staging: StagingStoreDirectory, StagingDir: "tmp"})
	require.NoError(err)
	loose, err := NewLooseStore(layout)
	require.NoError(err)
	opts := WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
		Compression: LooseCompressionOptions{
			Enabled: compressed,
		},
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(content)))
	b.ResetTimer()
	for range b.N {
		result, writeErr := loose.WriteBytes(b.Context(), content, opts)
		require.NoError(writeErr)
		b.StopTimer()
		require.NoError(loose.Remove(result.Hash, BestEffortRemoval))
		b.StartTimer()
	}
}

func BenchmarkCompressedLooseStreamingRead(b *testing.B) {
	require := require.New(b)
	content := representativeNDJSON(8_000)
	layout, err := NewLayout(b.TempDir(), LayoutOptions{Staging: StagingStoreDirectory, StagingDir: "tmp"})
	require.NoError(err)
	loose, err := NewLooseStore(layout)
	require.NoError(err)
	result, err := loose.WriteBytes(b.Context(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
		Compression: LooseCompressionOptions{
			Enabled: true,
		},
	})
	require.NoError(err)
	resolver := &mapResolver{locations: map[Hash]Location{result.Hash: {Member: true}}}
	store, err := NewStore(resolver, layout, StoreOptions{})
	require.NoError(err)
	b.Cleanup(func() { require.NoError(store.Close()) })
	buffer := make([]byte, 64<<10)
	b.ReportAllocs()
	b.SetBytes(int64(len(content)))
	b.ResetTimer()
	for range b.N {
		stream, _, openErr := store.OpenStream(b.Context(), result.Hash)
		require.NoError(openErr)
		_, copyErr := io.CopyBuffer(io.Discard, stream, buffer)
		require.NoError(copyErr)
		require.NoError(stream.Close())
	}
}

func BenchmarkCompressedLoosePackIngestion(b *testing.B) {
	require := require.New(b)
	content := bytes.Repeat([]byte("{\"type\":\"message\",\"content\":\"pack ingestion benchmark\"}\n"), 8_000)
	base := b.TempDir()
	b.ReportAllocs()
	b.SetBytes(int64(len(content)))
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		root, err := os.MkdirTemp(base, "iteration-") //nolint:usetesting // each benchmark iteration needs its own root outside b.TempDir cleanup ordering
		require.NoError(err)
		layout, err := NewLayout(root, LayoutOptions{Staging: StagingStoreDirectory, StagingDir: "tmp"})
		require.NoError(err)
		loose, err := NewLooseStore(layout)
		require.NoError(err)
		written, err := loose.WriteBytes(b.Context(), content, WriteOptions{
			Durability: AtomicPublication,
			Dedup:      VerifyFullHash,
			Compression: LooseCompressionOptions{
				Enabled: true,
			},
		})
		require.NoError(err)
		catalog := newMaintenanceCatalog()
		catalog.members[written.Hash] = Reference{
			Hash: written.Hash, OriginalHashes: []string{written.Hash.String()},
		}
		catalog.candidates[written.Hash] = Candidate{
			Hash: written.Hash, OriginalHashes: []string{written.Hash.String()},
			Paths: []string{filepath.Clean(written.Path)}, Size: written.Size,
		}
		maintainer, err := NewMaintainer(catalog, layout, MaintainerOptions{})
		require.NoError(err)
		b.StartTimer()

		stats, packErr := maintainer.Pack(b.Context(), PackOptions{})
		require.NoError(packErr)
		require.Equal(1, stats.BlobsPacked)

		b.StopTimer()
		require.NoError(maintainer.Close())
		require.NoError(os.RemoveAll(root))
		b.StartTimer()
	}
}

// Set TMPDIR to disk-backed storage to measure durable filesystem costs.
func BenchmarkLooseStreamDurableWrite(b *testing.B) {
	for _, compressed := range []bool{false, true} {
		for _, duplicate := range []bool{false, true} {
			b.Run(fmt.Sprintf("compressed=%t/duplicate=%t", compressed, duplicate), func(b *testing.B) {
				store := newLooseStoreForTest(b, StagingStoreDirectory)
				content := bytes.Repeat([]byte("streaming benchmark content\n"), 320)
				opts := WriteOptions{
					Durability:  DurablePublication,
					Dedup:       VerifyTypeAndSize,
					Compression: LooseCompressionOptions{Enabled: compressed},
				}
				if duplicate {
					_, err := store.WriteBytes(b.Context(), content, opts)
					require.NoError(b, err)
				}
				b.SetBytes(int64(len(content)))
				b.ReportAllocs()
				var sequence uint64
				for b.Loop() {
					if !duplicate {
						binary.LittleEndian.PutUint64(content, sequence)
						sequence++
					}
					result, err := store.Write(b.Context(), bytes.NewReader(content), opts)
					require.NoError(b, err)
					require.Equal(b, !duplicate, result.Created)
					require.Equal(b, int64(len(content)), result.Size)
				}
			})
		}
	}
}
