package packstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOldLayoutRawLooseObjectNeedsNoMigration(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	layout := layoutForStoreTest(t)
	content := bytes.Repeat([]byte("raw loose bytes written before compressed storage existed\n"), 64)
	hash := hashForTest(content)
	rawPath := layout.LoosePath(hash)
	require.NoError(os.MkdirAll(filepath.Dir(rawPath), 0o700))
	require.NoError(os.WriteFile(rawPath, content, 0o600))

	catalog := newMaintenanceCatalog()
	addMaintenanceCandidate(catalog, Candidate{
		Hash: hash, Paths: []string{rawPath}, Size: int64(len(content)),
	})
	store := newStoreForTest(t, catalog, layout)
	got, size := readStoreTest(t, store, hash)
	assert.Equal(content, got)
	assert.Equal(int64(len(content)), size)

	loose, err := NewLooseStore(layout)
	require.NoError(err)
	deduplicated, err := loose.WriteBytes(ctx, content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
		Compression: LooseCompressionOptions{
			Enabled: true,
		},
	})
	require.NoError(err)
	assert.False(deduplicated.Created)
	assert.Equal(LooseEncodingRaw, deduplicated.Encoding)
	assert.Equal(rawPath, deduplicated.Path)
	assert.NoFileExists(layout.CompressedLoosePath(hash))

	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	stats, err := maintainer.Pack(ctx, PackOptions{})
	require.NoError(err)
	assert.Equal(1, stats.BlobsPacked)
	assert.NoFileExists(rawPath)
	got, size = readStoreTest(t, maintainer.Store(), hash)
	assert.Equal(content, got)
	assert.Equal(int64(len(content)), size)

	// A redundant old-layout copy can still be removed without disturbing the
	// authoritative packed object.
	require.NoError(os.WriteFile(rawPath, content, 0o600))
	require.NoError(loose.Remove(hash, BestEffortRemoval))
	assert.NoFileExists(rawPath)
	got, _ = readStoreTest(t, maintainer.Store(), hash)
	assert.Equal(content, got)

	// Dropping pack metadata makes the same catalog member loose again. It can
	// then be rewritten using the new representation without a membership or
	// migration record.
	require.NoError(catalog.ClearPackMetadata(ctx))
	rewritten, err := loose.WriteBytes(ctx, content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
		Compression: LooseCompressionOptions{
			Enabled: true,
		},
	})
	require.NoError(err)
	assert.True(rewritten.Created)
	assert.Equal(LooseEncodingZstd, rewritten.Encoding)
	assert.FileExists(layout.CompressedLoosePath(hash))
	got, size = readStoreTest(t, maintainer.Store(), hash)
	assert.Equal(content, got)
	assert.Equal(int64(len(content)), size)
}

func TestLooseCompressionPreservesRepresentativeLogicalIdentity(t *testing.T) {
	tests := []struct {
		name    string
		content []byte
	}{
		{name: "JSON", content: representativeJSON(4_000)},
		{name: "NDJSON", content: representativeNDJSON(8_000)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			layout := layoutForStoreTest(t)
			loose, err := NewLooseStore(layout)
			require.NoError(err)
			result, err := loose.WriteBytes(t.Context(), tt.content, WriteOptions{
				Durability: AtomicPublication,
				Dedup:      VerifyFullHash,
				Compression: LooseCompressionOptions{
					Enabled: true,
				},
			})
			require.NoError(err)
			assert.Equal(LooseEncodingZstd, result.Encoding)
			assert.Less(result.StoredSize, result.Size/4, "representative structured data should save at least 75 percent")
			expectedDigest := sha256.Sum256(tt.content)
			assert.Equal(hex.EncodeToString(expectedDigest[:]), result.Hash.String())

			store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{
				result.Hash: {Member: true},
			}}, layout)
			stream, logicalSize, err := store.OpenStream(t.Context(), result.Hash)
			require.NoError(err)
			decodedDigest := sha256.New()
			decodedSize, err := io.Copy(decodedDigest, stream)
			require.NoError(err)
			require.NoError(stream.Close())
			assert.Equal(result.Size, logicalSize)
			assert.Equal(result.Size, decodedSize)
			assert.Equal(expectedDigest[:], decodedDigest.Sum(nil))
		})
	}
}

func TestCompressedLooseStreamingAllocationsArePayloadSizeIndependent(t *testing.T) {
	if testing.Short() {
		t.Skip("measures allocations while streaming multi-megabyte loose objects")
	}
	small := bytes.Repeat([]byte("{\"kind\":\"allocation\",\"value\":123456789}\n"), 100_000)
	large := bytes.Repeat([]byte("{\"kind\":\"allocation\",\"value\":123456789}\n"), 400_000)

	smallBytes := compressedLooseReadBytesPerOp(t, small)
	largeBytes := compressedLooseReadBytesPerOp(t, large)
	t.Logf("verified compressed streaming allocations: small=%d B/op large=%d B/op", smallBytes, largeBytes)

	assert.LessOrEqual(t, largeBytes, smallBytes+1<<20,
		"stream allocations should be bounded by codec windows and buffers, not decoded payload length")
	assert.LessOrEqual(t, largeBytes, int64(4<<20),
		"new compressed loose streams should stay within the fixed-window allocation budget")
}

func TestCompressedLooseReadsLegacyLargerWindow(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	content := bytes.Repeat([]byte("legacy-window-content-0123456789\n"), 100_000)
	layout := layoutForStoreTest(t)
	hash := hashForTest(content)
	var physical bytes.Buffer
	header := encodeCompressedLooseHeader(uint64(len(content)))
	_, err := physical.Write(header[:])
	require.NoError(err)
	encoder, err := zstd.NewWriter(&physical,
		zstd.WithEncoderConcurrency(1),
		zstd.WithWindowSize(8<<20),
	)
	require.NoError(err)
	_, err = encoder.Write(content)
	require.NoError(err)
	require.NoError(encoder.Close())
	var frameHeader zstd.Header
	require.NoError(frameHeader.Decode(physical.Bytes()[compressedLooseHeaderSize:]))
	assert.False(frameHeader.SingleSegment)
	assert.Greater(frameHeader.WindowSize, uint64(looseZstdWindowBytes),
		"fixture must exercise a frame written before the fixed-window policy")
	path := layout.CompressedLoosePath(hash)
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(os.WriteFile(path, physical.Bytes(), 0o600))

	store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{
		hash: {Member: true},
	}}, layout)
	stream, size, err := store.OpenStream(t.Context(), hash)
	require.NoError(err)
	got, err := io.ReadAll(stream)
	require.NoError(err)
	require.NoError(stream.Close())
	assert.Equal(int64(len(content)), size)
	assert.Equal(content, got)
}

func compressedLooseReadBytesPerOp(t *testing.T, content []byte) int64 {
	t.Helper()
	layout := layoutForStoreTest(t)
	loose, err := NewLooseStore(layout)
	require.NoError(t, err)
	result, err := loose.WriteBytes(t.Context(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
		Compression: LooseCompressionOptions{
			Enabled: true,
		},
	})
	require.NoError(t, err)
	require.Equal(t, LooseEncodingZstd, result.Encoding)
	store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{
		result.Hash: {Member: true},
	}}, layout)

	resultBenchmark := testing.Benchmark(func(b *testing.B) {
		b.Helper()
		buffer := make([]byte, 64<<10)
		b.ReportAllocs()
		b.SetBytes(int64(len(content)))
		for range b.N {
			stream, _, openErr := store.OpenStream(t.Context(), result.Hash)
			require.NoError(b, openErr)
			_, copyErr := io.CopyBuffer(io.Discard, stream, buffer)
			require.NoError(b, copyErr)
			require.NoError(b, stream.Close())
		}
	})
	return resultBenchmark.AllocedBytesPerOp()
}

func representativeJSON(records int) []byte {
	var builder strings.Builder
	builder.Grow(records * 96)
	builder.WriteString("{\"sessions\":[")
	for index := range records {
		if index != 0 {
			builder.WriteByte(',')
		}
		_, _ = fmt.Fprintf(&builder,
			`{"id":"session-%06d","agent":"codex","status":"complete","message":"repeatable structured content"}`,
			index,
		)
	}
	builder.WriteString("]}")
	return []byte(builder.String())
}

func representativeNDJSON(records int) []byte {
	var builder strings.Builder
	builder.Grow(records * 112)
	for index := range records {
		_, _ = fmt.Fprintf(&builder,
			"{\"sequence\":%d,\"role\":\"assistant\",\"type\":\"message\",\"content\":\"repeatable structured content\"}\n",
			index,
		)
	}
	return []byte(builder.String())
}
