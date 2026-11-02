// Package pack implements a content-addressed blob/pack container format for
// archive and backup tools: blobs are stored in sealed-immutable pack files,
// written once via Writer and never mutated afterward, with optional zstd
// compression and XChaCha20-Poly1305 encryption.
//
// Buffered Append and ReadBlob remain the compatibility path. AppendStream,
// PrepareBlob, AppendPrepared, and Reader.OpenBlob stream plain format-v1
// frames through bounded scratch and codec windows. A BlobReader is trusted
// only after terminal io.EOF, Verify succeeds, or Verified reports true;
// closing it earlier reports ErrVerificationIncomplete. Format-v1 encryption
// authenticates whole frames and therefore returns ErrStreamUnsupported from
// the streaming APIs instead of exposing unauthenticated prefixes.
//
// Preparation uses private disk scratch rather than object-sized heap. Its
// worst case is the raw input plus an incompressible zstd candidate, about
// 2.004 times raw length plus fixed frame overhead. AppendStreamOptions should
// bound and place that scratch explicitly for automatic work. Context
// cancellation and typed StreamLimitError values fail closed and clean only
// scratch owned by the operation.
//
// Format v1 retains its 4 GiB raw-length ceiling. Streaming changes transport
// and allocation behavior, not the on-disk representation or that ceiling.
package pack

import "errors"

const (
	// FormatVersion is the pack container format version (backup/FORMAT.md, Pack Files).
	FormatVersion = 1

	// DefaultTargetSize is the advisory pack size at which callers seal.
	DefaultTargetSize = 32 << 20

	// DefaultZstdLevel is the default zstd compression level (backup/FORMAT.md, Pack Files).
	DefaultZstdLevel = 3

	headerMagic  = "MVPK"
	trailerMagic = "KPVM"
	headerSize   = 6

	// MaxFooterLen bounds the encoded footer before allocation.
	MaxFooterLen = 1 << 30
	maxFooterLen = MaxFooterLen

	// MaxRawLen bounds the raw (decompressed) length of one blob. It matches
	// the zstd decoder's WithDecoderMaxMemory(1<<32) limit in frame.go: a
	// compressed frame whose entry claims a larger raw length can never
	// decode successfully, so decodeFrame rejects it before trusting the
	// value to size a preallocation. Append enforces the same bound on raw
	// input so every pack Append writes can produce is guaranteed readable.
	// Exported so callers that buffer whole files before appending can reject
	// oversized input from a cheap stat instead of reading it into memory
	// first.
	MaxRawLen = 1 << 32

	// MinEntryOffset is the first valid byte offset for a blob frame.
	MinEntryOffset = headerSize

	// MaxStoredLen bounds one stored frame before allocation.
	MaxStoredLen = maxStoredLen

	// maxStoredLen bounds StoredLen, the number of bytes a footer entry claims
	// occupy the pack's data region: readStored preallocates a buffer of this
	// size before any integrity check runs, so an untrusted footer entry must
	// not be able to claim an arbitrarily large StoredLen (bounded only by
	// file size otherwise). The legitimate maximum is MaxRawLen inflated by
	// the worst-case expansion a frame can add on top of the raw bytes:
	//   - zstd's documented worst case for incompressible input is
	//     input + (input >> 8) + a small fixed number of frame/block header
	//     bytes; maxZstdFrameOverhead is a round, conservative stand-in for
	//     that fixed term.
	//   - an encrypted pack additionally seals the frame, adding a fixed
	//     XChaCha20-Poly1305 overhead (24-byte nonce + 16-byte tag; see
	//     maxSealOverhead in crypter.go).
	// Both allowances can apply to the same entry (compress then seal), so
	// they're additive.
	maxStoredLen = MaxRawLen + MaxRawLen>>8 + maxZstdFrameOverhead + maxSealOverhead

	// maxZstdFrameOverhead is the conservative fixed-byte allowance in
	// maxStoredLen for zstd frame/block headers, on top of the input+input>>8
	// expansion term.
	maxZstdFrameOverhead = 64
)

// BlobFlags describes how a blob's stored bytes were produced.
type BlobFlags uint8

const (
	// BlobCompressed marks a zstd-compressed frame.
	BlobCompressed BlobFlags = 1 << 0
	// BlobEncrypted marks an AEAD-sealed frame.
	BlobEncrypted BlobFlags = 1 << 1
)

type packFlags uint8

const packEncrypted packFlags = 1 << 0

var (
	ErrBadMagic           = errors.New("pack: bad magic")
	ErrUnsupportedVersion = errors.New("pack: unsupported format version")
	ErrTruncated          = errors.New("pack: truncated file")
	ErrChecksum           = errors.New("pack: footer checksum mismatch")
	ErrCorrupt            = errors.New("pack: corrupt")
	ErrBlobMismatch       = errors.New("pack: blob content hash mismatch")
	ErrEncrypted          = errors.New("pack: encrypted pack requires a crypter")
	ErrDecrypt            = errors.New("pack: decryption failed")
	ErrSealed             = errors.New("pack: writer already sealed")
)
