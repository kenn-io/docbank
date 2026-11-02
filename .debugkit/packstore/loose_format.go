package packstore

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"

	"go.kenn.io/kit/pack"
)

const (
	compressedLooseMagic      = "KPLZ"
	compressedLooseVersion    = 1
	compressedLooseHeaderSize = 16
)

func encodeCompressedLooseHeader(logicalSize uint64) [compressedLooseHeaderSize]byte {
	var header [compressedLooseHeaderSize]byte
	copy(header[:4], compressedLooseMagic)
	header[4] = compressedLooseVersion
	binary.LittleEndian.PutUint64(header[8:], logicalSize)
	return header
}

func decodeCompressedLooseHeader(header []byte) (int64, error) {
	if len(header) != compressedLooseHeaderSize {
		return 0, fmt.Errorf("%w: compressed loose header is %d bytes, want %d", pack.ErrCorrupt, len(header), compressedLooseHeaderSize)
	}
	if !bytes.Equal(header[:4], []byte(compressedLooseMagic)) {
		return 0, fmt.Errorf("%w: compressed loose header", pack.ErrBadMagic)
	}
	if header[4] != compressedLooseVersion {
		return 0, fmt.Errorf("%w: compressed loose version %d", pack.ErrUnsupportedVersion, header[4])
	}
	if header[5] != 0 || header[6] != 0 || header[7] != 0 {
		return 0, fmt.Errorf("%w: compressed loose reserved bytes are non-zero", pack.ErrCorrupt)
	}
	logicalSize := binary.LittleEndian.Uint64(header[8:])
	if logicalSize > math.MaxInt64 {
		return 0, fmt.Errorf("%w: compressed loose logical size %d exceeds %d", pack.ErrCorrupt, logicalSize, uint64(math.MaxInt64))
	}
	return int64(logicalSize), nil
}
