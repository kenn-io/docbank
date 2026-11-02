package packstore

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLooseCompressionIndependentStreams(t *testing.T) {
	t.Parallel()
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	opts := WriteOptions{
		Durability: AtomicPublication, Dedup: VerifyTypeAndSize,
		Compression: LooseCompressionOptions{Enabled: true, MinSavingsPercent: 10},
	}
	for worker := range 4 {
		t.Run(fmt.Sprintf("writer=%d", worker), func(t *testing.T) {
			t.Parallel()
			for i := range 12 {
				size := []int{8192, 256 << 10, 2 << 20}[i%3]
				content := bytes.Repeat([]byte{'a'}, size)
				wantEncoding := LooseEncodingZstd
				if i%2 == 1 {
					content = deterministicLooseNoise(size)
					wantEncoding = LooseEncodingRaw
				}
				content[0], content[1] = byte(worker), byte(i)
				// An interrupted source must not contaminate the next stream.
				_, err := store.Write(t.Context(), io.MultiReader(
					bytes.NewReader(content[:size/2]), &errorReader{err: errInjectedPrimary},
				), opts)
				require.ErrorIs(t, err, errInjectedPrimary)

				result, err := store.Write(t.Context(), bytes.NewReader(content), opts)
				require.NoError(t, err)
				assert.Equal(t, wantEncoding, result.Encoding)
				// Full verification decodes the stored stream and checks its
				// bytes against the independently computed source identity.
				_, found, err := store.Verify(hashForTest(content), int64(size),
					VerifyFullHash, AtomicPublication)
				require.NoError(t, err)
				require.True(t, found)
			}
		})
	}
}

func BenchmarkLooseWriteDurableCompressed(b *testing.B) {
	for _, size := range []int{8192, 1 << 20} {
		for _, compressible := range []bool{false, true} {
			b.Run(fmt.Sprintf("bytes=%d/compressible=%t", size, compressible), func(b *testing.B) {
				store := newLooseStoreForTest(b, StagingStoreDirectory)
				content := deterministicLooseNoise(size)
				if compressible {
					content = bytes.Repeat([]byte{'a'}, size)
				}
				opts := WriteOptions{
					Durability: DurablePublication, Dedup: VerifyTypeAndSize,
					Compression: LooseCompressionOptions{Enabled: true, MinSavingsPercent: 10},
				}
				b.SetBytes(int64(size))
				b.ReportAllocs()
				var sequence uint64
				for b.Loop() {
					binary.LittleEndian.PutUint64(content, sequence)
					sequence++
					result, err := store.Write(b.Context(), bytes.NewReader(content), opts)
					require.NoError(b, err)
					require.True(b, result.Created)
				}
			})
		}
	}
}
