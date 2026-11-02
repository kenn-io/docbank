package pack

import (
	"encoding/binary"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Blobs above maxSingleSegmentLen must carry an explicit window descriptor so
// readers need not hold a window as large as the whole blob.
func TestEncodeFrameAvoidsSingleSegmentAboveLimit(t *testing.T) {
	old := maxSingleSegmentLen
	maxSingleSegmentLen = 64 << 10
	t.Cleanup(func() { maxSingleSegmentLen = old })

	tests := []struct {
		name          string
		size          int
		singleSegment bool
	}{
		{"at the limit", maxSingleSegmentLen, true},
		{"one byte over the limit", maxSingleSegmentLen + 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := compressibleBytes(tt.size)
			stored, compressed := encodeFrame(raw, DefaultZstdLevel)
			require.True(t, compressed)

			var header zstd.Header
			require.NoError(t, header.Decode(stored))
			assert.Equal(t, tt.singleSegment, header.SingleSegment)

			decoded, err := decodeFrame(stored, compressed, uint64(len(raw)))
			require.NoError(t, err)
			assert.Equal(t, raw, decoded)
		})
	}
}

// Packs written before maxSingleSegmentLen existed hold single-segment frames
// of any size up to MaxRawLen, whose window is their whole content size. Both
// read paths must accept windows beyond zstd's 512 MiB default. The frame's
// header claims more content than it holds, so the window check runs without
// allocating that much: decoding gets past it and then fails on the size
// mismatch instead.
func TestReaderAcceptsSingleSegmentFramesOverZstdDefaultWindow(t *testing.T) {
	if strconv.IntSize == 32 {
		t.Skip("32-bit readers keep the 512 MiB window ceiling")
	}
	frame := zstdEncoder(DefaultZstdLevel, true).EncodeAll(compressibleBytes(1<<20), nil)
	var header zstd.Header
	require.NoError(t, header.Decode(frame))
	require.True(t, header.SingleSegment)
	require.Equal(t, 4, header.HeaderSize-5, "expected a 4-byte content size field")
	rawLen := uint64(zstd.MaxWindowSize) + 1
	// Magic number (4 bytes) and frame descriptor (1 byte) precede the
	// content size; single-segment frames have no window descriptor.
	binary.LittleEndian.PutUint32(frame[5:9], uint32(rawLen))
	require.NoError(t, header.Decode(frame))
	require.Equal(t, rawLen, header.FrameContentSize)

	t.Run("decodeFrame", func(t *testing.T) {
		_, err := decodeFrame(frame, true, rawLen)
		require.ErrorIs(t, err, zstd.ErrFrameSizeMismatch)
	})

	t.Run("OpenBlob", func(t *testing.T) {
		require := require.New(t)
		dir := t.TempDir()
		writer, err := NewWriter(dir, WriterOptions{})
		require.NoError(err)
		_, err = writer.AppendEncoded(ComputeBlobID([]byte("oversized")), frame, rawLen, true)
		require.NoError(err)
		final := filepath.Join(dir, writer.ID()+".pack")
		entries, err := writer.Seal(final)
		require.NoError(err)

		reader, err := OpenReader(final, nil)
		require.NoError(err)
		t.Cleanup(func() { require.NoError(reader.Close()) })
		blob, err := reader.OpenBlob(t.Context(), entries[0])
		require.NoError(err)
		require.ErrorIs(blob.Close(), ErrVerificationIncomplete)
	})
}

func compressibleBytes(n int) []byte {
	raw := make([]byte, n)
	for i := range raw {
		raw[i] = byte(i % 251)
	}
	return raw
}
