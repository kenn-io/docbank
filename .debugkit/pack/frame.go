package pack

import (
	"fmt"
	"strconv"
	"sync"

	"github.com/klauspost/compress/zstd"
)

type zstdEncoderKey struct {
	level         int
	singleSegment bool
}

// Shared zstd codecs. EncodeAll/DecodeAll are safe for concurrent use on a
// single Encoder/Decoder, so one instance per level and framing mode serves
// all writers.
var (
	zstdEncMu sync.Mutex
	zstdEncs  = map[zstdEncoderKey]*zstd.Encoder{}
	zstdDec   = func() *zstd.Decoder {
		d, err := zstd.NewReader(nil,
			zstd.WithDecoderConcurrency(0), zstd.WithDecoderMaxMemory(1<<32),
			// Single-segment frames carry a window equal to their content
			// size; the 512 MiB default would reject blobs this package
			// itself produced.
			zstd.WithDecoderMaxWindow(maxDecoderWindow()))
		if err != nil {
			panic(fmt.Sprintf("pack: initializing zstd decoder: %v", err))
		}
		return d
	}()
)

func maxDecoderWindow() uint64 {
	// The codec uses int for history-buffer sizes and adds space beyond the
	// window. Retain its default on 32-bit systems to avoid integer overflow.
	if strconv.IntSize == 32 {
		return zstd.MaxWindowSize
	}
	return MaxRawLen
}

func zstdEncoder(level int, singleSegment bool) *zstd.Encoder {
	if level <= 0 {
		level = DefaultZstdLevel
	}
	key := zstdEncoderKey{level: level, singleSegment: singleSegment}
	zstdEncMu.Lock()
	defer zstdEncMu.Unlock()
	if enc, ok := zstdEncs[key]; ok {
		return enc
	}
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)),
		zstd.WithSingleSegment(singleSegment))
	if err != nil {
		panic(fmt.Sprintf("pack: initializing zstd encoder level %d: %v", level, err))
	}
	zstdEncs[key] = enc
	return enc
}

// minCompressionSavings returns the minimum number of bytes zstd must save
// for a compressed frame to be stored instead of raw: at least 3% of rawLen
// rounded up (ceil(rawLen*3/100)), with a floor of 1 byte so a saving of zero
// never counts. Rounding up, rather than truncating, matters at sizes like
// rawLen=99 where a 2-byte saving is only 2.02% and must not qualify.
func minCompressionSavings(rawLen int) int {
	return max(1, (rawLen*3+99)/100)
}

// maxSingleSegmentLen is the largest blob encodeFrame writes as a
// single-segment frame. Single-segment frames use their content size as the
// window. Above the klauspost decoder's default 512 MiB limit, use an explicit
// window instead. Tests lower it to exercise the boundary with small blobs.
var maxSingleSegmentLen = zstd.MaxWindowSize

// encodeFrame trial-compresses raw. It returns the compressed frame only when
// zstd saves at least 3% (backup/FORMAT.md, Pack Files); otherwise it returns raw as-is.
func encodeFrame(raw []byte, level int) (stored []byte, compressed bool) {
	// Legacy bounded readers cap decoder memory at RawLen. A zstd frame always
	// needs at least MinWindowSize, so smaller blobs must remain raw for
	// downgrade compatibility.
	if len(raw) < zstd.MinWindowSize {
		return raw, false
	}
	singleSegment := len(raw) <= maxSingleSegmentLen
	encoder := zstdEncoder(level, singleSegment)
	c := encoder.EncodeAll(raw, make([]byte, 0, len(raw)))
	minSavings := minCompressionSavings(len(raw))
	if len(c) > len(raw)-minSavings {
		return raw, false
	}
	return c, true
}

// EncodeFrame is the exported form of the frame encoding Writer.Append
// performs, for callers that compress blobs concurrently and hand the
// result to Writer.AppendEncoded. It is safe for concurrent use.
func EncodeFrame(raw []byte, level int) (stored []byte, compressed bool) {
	return encodeFrame(raw, level)
}

// maxFramePrealloc caps how many bytes decodeFrame preallocates for a
// compressed frame's output buffer. rawLen comes from an untrusted footer
// entry, so it must not be trusted as an allocation size directly; DecodeAll
// grows the buffer as needed, and the length check below still catches any
// mismatch between the decoded size and rawLen.
const maxFramePrealloc = 4 << 20

// decodeFrame reverses encodeFrame and validates the expected raw length.
// The MaxRawLen bound applies to every frame, not only compressed ones:
// parseFooterRegion already rejects oversized entries at footer parse time,
// and this check backstops any caller that constructs an Entry some other
// way.
func decodeFrame(stored []byte, compressed bool, rawLen uint64) ([]byte, error) {
	if rawLen > MaxRawLen {
		return nil, fmt.Errorf("%w: raw length %d exceeds maximum %d",
			ErrCorrupt, rawLen, uint64(MaxRawLen))
	}
	raw := stored
	if compressed {
		var err error
		raw, err = zstdDec.DecodeAll(stored, make([]byte, 0, min(rawLen, maxFramePrealloc)))
		if err != nil {
			return nil, fmt.Errorf("%w: zstd decode: %w", ErrCorrupt, err)
		}
	}
	if uint64(len(raw)) != rawLen {
		return nil, fmt.Errorf("%w: frame decoded to %d bytes, expected %d",
			ErrCorrupt, len(raw), rawLen)
	}
	return raw, nil
}
