package blob

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkSmallBlobWrite includes durable publication or deduplication on
// disk. Set TMPDIR to disk-backed storage when measuring filesystem costs.
func BenchmarkSmallBlobWrite(b *testing.B) {
	for _, size := range []int{1024, 4096} {
		for _, existing := range []bool{false, true} {
			b.Run(fmt.Sprintf("bytes=%d/existing=%t", size, existing), func(b *testing.B) {
				bs, err := newReaderStoreWithOptions(alwaysMemberResolver{}, b.TempDir(), ManagedOptions())
				require.NoError(b, err)
				b.Cleanup(func() { require.NoError(b, bs.Close()) })
				content := bytes.Repeat([]byte("x"), size)
				if existing {
					_, err := bs.WriteDetailedContext(b.Context(), bytes.NewReader(content))
					require.NoError(b, err)
				}
				b.SetBytes(int64(size))
				b.ReportAllocs()
				var sequence uint64
				for b.Loop() {
					if !existing {
						binary.LittleEndian.PutUint64(content, sequence)
						sequence++
					}
					receipt, err := bs.WriteDetailedContext(b.Context(), bytes.NewReader(content))
					require.NoError(b, err)
					require.Equal(b, !existing, receipt.Created)
					require.Equal(b, int64(size), receipt.Size)
				}
			})
		}
	}
}
