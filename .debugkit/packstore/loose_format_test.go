package packstore

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompressedLooseHeaderRoundTrips(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	const logicalSize = uint64(0x0102030405060708)

	header := encodeCompressedLooseHeader(logicalSize)

	assert.Len(header, compressedLooseHeaderSize)
	assert.Equal([]byte{
		'K', 'P', 'L', 'Z',
		1, 0, 0, 0,
		0x08, 0x07, 0x06, 0x05, 0x04, 0x03, 0x02, 0x01,
	}, header[:])
	decodedSize, err := decodeCompressedLooseHeader(header[:])
	require.NoError(err)
	assert.Equal(int64(logicalSize), decodedSize)
}

func TestCompressedLooseHeaderRoundTripsMaximumLogicalSize(t *testing.T) {
	header := encodeCompressedLooseHeader(uint64(math.MaxInt64))

	logicalSize, err := decodeCompressedLooseHeader(header[:])

	require.NoError(t, err)
	assert.Equal(t, int64(math.MaxInt64), logicalSize)
}

func TestCompressedLooseHeaderRejectsInvalidFields(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*[compressedLooseHeaderSize]byte)
	}{
		{
			name: "bad magic",
			mutate: func(header *[compressedLooseHeaderSize]byte) {
				header[0] = 'X'
			},
		},
		{
			name: "unknown version",
			mutate: func(header *[compressedLooseHeaderSize]byte) {
				header[4] = 2
			},
		},
		{
			name: "non-zero reserved byte 5",
			mutate: func(header *[compressedLooseHeaderSize]byte) {
				header[5] = 1
			},
		},
		{
			name: "non-zero reserved byte 6",
			mutate: func(header *[compressedLooseHeaderSize]byte) {
				header[6] = 1
			},
		},
		{
			name: "non-zero reserved byte 7",
			mutate: func(header *[compressedLooseHeaderSize]byte) {
				header[7] = 1
			},
		},
		{
			name: "logical size above int64",
			mutate: func(header *[compressedLooseHeaderSize]byte) {
				binary.LittleEndian.PutUint64(header[8:], uint64(math.MaxInt64)+1)
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			header := encodeCompressedLooseHeader(123)
			tt.mutate(&header)

			_, err := decodeCompressedLooseHeader(header[:])

			require.Error(t, err)
		})
	}
}

func TestCompressedLooseHeaderRejectsWrongLength(t *testing.T) {
	valid := encodeCompressedLooseHeader(123)

	_, err := decodeCompressedLooseHeader(valid[:compressedLooseHeaderSize-1])
	require.Error(t, err)
	_, err = decodeCompressedLooseHeader(append(valid[:], 0))
	require.Error(t, err)
}
