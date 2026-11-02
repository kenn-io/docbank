package packstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"

	"github.com/klauspost/compress/zstd"
	"go.kenn.io/kit/pack"
)

// ErrBlobTooLarge reports a bounded blob or pack that exceeds its limit.
var ErrBlobTooLarge = errors.New("packstore: blob exceeds bounded read limit")

// errUnsupportedMaintenanceEncoding preserves pack.ErrCorrupt for ordinary
// maintenance callers while allowing import planning to recognize settings it
// cannot consume directly.
var errUnsupportedMaintenanceEncoding = errors.New("packstore: unsupported maintenance pack encoding")

// LimitDimension identifies the bounded quantity that exceeded its ceiling.
type LimitDimension string

const (
	LimitBlobRawBytes       LimitDimension = "blob_raw_bytes"
	LimitBlobStoredBytes    LimitDimension = "blob_stored_bytes"
	LimitBlobWindowBytes    LimitDimension = "blob_window_bytes"
	LimitBlobStatBytes      LimitDimension = "blob_stat_bytes"
	LimitPackContainerBytes LimitDimension = "pack_container_bytes"
	LimitPackFooterBytes    LimitDimension = "pack_footer_bytes"
	LimitPackEntryCount     LimitDimension = "pack_entry_count"
)

// LimitError carries the exact bounded quantity and values.
type LimitError struct {
	Dimension LimitDimension
	Actual    uint64
	Limit     uint64
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("%s: %s is %d, limit %d", ErrBlobTooLarge, e.Dimension, e.Actual, e.Limit)
}

func (e *LimitError) Unwrap() error { return ErrBlobTooLarge }

func newLimitError(dimension LimitDimension, actual, limit uint64) error {
	return &LimitError{Dimension: dimension, Actual: actual, Limit: limit}
}

const (
	plainPackVersion     = byte(1)
	plainPackHeaderSize  = 6
	plainPackTrailerSize = 40
	plainPackEntrySize   = 61
)

var boundedCRC32CTable = crc32.MakeTable(crc32.Castagnoli)

var maxPlatformInt = uint64(^uint(0) >> 1)

var snapshotBoundedPackPathIdentity = snapshotPathIdentity

type boundedPackReader struct {
	file         *os.File
	entries      map[pack.BlobID]pack.Entry
	streamReader *pack.Reader
}

// MaintenancePackReader retains the exact descriptor preflighted against the
// supplied safety limits.
type MaintenancePackReader struct {
	reader *boundedPackReader
	limits Limits
}

// OpenMaintenancePack validates a stable plain-v1 pack before exposing reads.
func OpenMaintenancePack(path string, limits Limits) (*MaintenancePackReader, error) {
	if err := validateLimits(limits); err != nil {
		return nil, err
	}
	reader, err := openBoundedPack(path, limits)
	if err != nil {
		return nil, err
	}
	return &MaintenancePackReader{reader: reader, limits: limits}, nil
}

// Entries returns a copy of the authoritative footer entries.
func (r *MaintenancePackReader) Entries() []pack.Entry {
	entries := make([]pack.Entry, 0, len(r.reader.entries))
	for _, entry := range r.reader.entries {
		entries = append(entries, entry)
	}
	return entries
}

// ReadBlob verifies and reads one entry within the configured blob ceiling.
func (r *MaintenancePackReader) ReadBlob(hash Hash) ([]byte, error) {
	id, err := pack.ParseBlobID(hash.String())
	if err != nil {
		return nil, err
	}
	entry, ok := r.reader.entries[id]
	if !ok {
		return nil, fmt.Errorf("%w: blob %s is absent from pack footer", fs.ErrNotExist, hash)
	}
	return r.reader.readBlob(entry, r.limits.BlobBytes)
}

// OpenBlob opens a verified-on-EOF stream for an authoritative footer entry.
func (r *MaintenancePackReader) OpenBlob(ctx context.Context, hash Hash) (*pack.BlobReader, error) {
	id, err := pack.ParseBlobID(hash.String())
	if err != nil {
		return nil, err
	}
	entry, ok := r.reader.entries[id]
	if !ok {
		return nil, fmt.Errorf("%w: blob %s is absent from pack footer", fs.ErrNotExist, hash)
	}
	limit := uint64(r.limits.BlobBytes)
	if entry.RawLen > limit {
		return nil, newLimitError(LimitBlobRawBytes, entry.RawLen, limit)
	}
	if entry.StoredLen > limit {
		return nil, newLimitError(LimitBlobStoredBytes, entry.StoredLen, limit)
	}
	if r.reader.streamReader == nil {
		return nil, errors.New("packstore: maintenance reader does not support streaming")
	}
	return r.reader.streamReader.OpenBlobWithOptions(ctx, entry, pack.BlobReaderOptions{
		WindowBytes: uint64(max(r.limits.BlobBytes, int64(1<<10))),
	})
}

// Close releases the retained pack descriptor.
func (r *MaintenancePackReader) Close() error { return r.reader.Close() }

func validateLimits(limits Limits) error {
	if limits.BlobBytes < 0 || limits.PackBytes <= 0 || limits.FooterBytes <= 0 || limits.PackEntries <= 0 {
		return errors.New("packstore: invalid maintenance limits")
	}
	return nil
}

func openBoundedPack(path string, limits Limits) (*boundedPackReader, error) {
	pathInfo, err := snapshotBoundedPackPathIdentity(path)
	if err != nil {
		return nil, fmt.Errorf("inspect pack for bounded preflight: %w", err)
	}
	if err := validateRegularNoFollow(path, pathInfo); err != nil {
		return nil, fmt.Errorf("validate pack for bounded preflight: %w", err)
	}
	f, err := openNoFollow(path, false)
	if err != nil {
		return nil, fmt.Errorf("open pack for bounded preflight: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("stat pack for bounded preflight: %w", err), f.Close())
	}
	if !os.SameFile(pathInfo, info) {
		return nil, errors.Join(errors.New("packstore: pack changed identity during bounded preflight"), f.Close())
	}
	reader, err := openBoundedPackFile(f, limits)
	if err != nil {
		return nil, err
	}
	return reader, nil
}

// openBoundedPackFile validates a pack through an already-open descriptor. It
// takes ownership of f whether validation succeeds or fails.
func openBoundedPackFile(f *os.File, limits Limits) (*boundedPackReader, error) {
	if f == nil {
		return nil, errors.New("packstore: nil bounded pack file")
	}
	keepOpen := false
	defer func() {
		if !keepOpen {
			_ = f.Close()
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat pack for bounded preflight: %w", err)
	}
	size := info.Size()
	if size > limits.PackBytes {
		return nil, newLimitError(LimitPackContainerBytes, uint64(size), uint64(limits.PackBytes))
	}
	if size < plainPackHeaderSize+plainPackTrailerSize {
		return nil, fmt.Errorf("%w: %d bytes is too small for a plain pack", pack.ErrTruncated, size)
	}
	var header [plainPackHeaderSize]byte
	if err := readBoundedPackAt(f, header[:], 0, "header"); err != nil {
		return nil, err
	}
	if !bytes.Equal(header[:4], []byte("MVPK")) {
		return nil, fmt.Errorf("%w: header", pack.ErrBadMagic)
	}
	if header[4] != plainPackVersion {
		return nil, fmt.Errorf("%w: version %d", pack.ErrUnsupportedVersion, header[4])
	}
	if header[5] != 0 {
		return nil, fmt.Errorf("%w: %w: bounded reads require plain v1 flags, got %#x",
			pack.ErrCorrupt, errUnsupportedMaintenanceEncoding, header[5])
	}

	var trailer [plainPackTrailerSize]byte
	if err := readBoundedPackAt(f, trailer[:], size-plainPackTrailerSize, "trailer"); err != nil {
		return nil, err
	}
	if !bytes.Equal(trailer[36:], []byte("KPVM")) {
		return nil, fmt.Errorf("%w: trailer", pack.ErrBadMagic)
	}
	footerLen := uint64(binary.LittleEndian.Uint32(trailer[:4]))
	if footerLen > uint64(limits.FooterBytes) {
		return nil, newLimitError(LimitPackFooterBytes, footerLen, uint64(limits.FooterBytes))
	}
	if footerLen > pack.MaxFooterLen {
		return nil, newLimitError(LimitPackFooterBytes, footerLen, pack.MaxFooterLen)
	}
	if footerLen > maxPlatformInt {
		return nil, newLimitError(LimitPackFooterBytes, footerLen, maxPlatformInt)
	}
	fileSize := uint64(size)
	if footerLen < 4 || fileSize < plainPackHeaderSize+plainPackTrailerSize+footerLen {
		return nil, fmt.Errorf("%w: footer length %d is outside %d-byte pack", pack.ErrTruncated, footerLen, size)
	}
	footerStart := fileSize - plainPackTrailerSize - footerLen
	var countBytes [4]byte
	if err := readBoundedPackAt(f, countBytes[:], int64(footerStart), "footer count"); err != nil {
		return nil, err
	}
	count := uint64(binary.LittleEndian.Uint32(countBytes[:]))
	if count > uint64(limits.PackEntries) {
		return nil, newLimitError(LimitPackEntryCount, count, uint64(limits.PackEntries))
	}
	wantFooterLen := uint64(4) + count*plainPackEntrySize
	if footerLen != wantFooterLen {
		return nil, fmt.Errorf("%w: footer length %d, want %d for %d entries", pack.ErrCorrupt, footerLen, wantFooterLen, count)
	}
	footer := make([]byte, int(footerLen))
	if err := readBoundedPackAt(f, footer, int64(footerStart), "footer"); err != nil {
		return nil, err
	}
	digest := sha256.New()
	_, _ = digest.Write(footer)
	_, _ = digest.Write(trailer[:4])
	if !bytes.Equal(digest.Sum(nil), trailer[4:36]) {
		return nil, pack.ErrChecksum
	}

	entries := make(map[pack.BlobID]pack.Entry, int(count))
	for i := range int(count) {
		offset := 4 + i*plainPackEntrySize
		var entry pack.Entry
		copy(entry.ID[:], footer[offset:offset+32])
		entry.Offset = binary.LittleEndian.Uint64(footer[offset+32:])
		entry.StoredLen = binary.LittleEndian.Uint64(footer[offset+40:])
		entry.RawLen = binary.LittleEndian.Uint64(footer[offset+48:])
		entry.Flags = pack.BlobFlags(footer[offset+56])
		entry.CRC32C = binary.LittleEndian.Uint32(footer[offset+57:])
		if entry.RawLen > pack.MaxRawLen {
			return nil, fmt.Errorf("%w: entry %d raw length %d exceeds format maximum", pack.ErrCorrupt, i, entry.RawLen)
		}
		if entry.StoredLen > pack.MaxStoredLen {
			return nil, fmt.Errorf("%w: entry %d stored length %d exceeds format maximum", pack.ErrCorrupt, i, entry.StoredLen)
		}
		if entry.Flags&^pack.BlobCompressed != 0 {
			return nil, fmt.Errorf("%w: entry %d has unsupported flags %#x", pack.ErrCorrupt, i, entry.Flags)
		}
		end := entry.Offset + entry.StoredLen
		if entry.Offset < plainPackHeaderSize || end < entry.Offset || end > footerStart {
			return nil, fmt.Errorf("%w: entry %d span is outside data region", pack.ErrCorrupt, i)
		}
		if _, duplicate := entries[entry.ID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate blob id %s", pack.ErrCorrupt, entry.ID)
		}
		entries[entry.ID] = entry
	}
	streamReader, err := pack.NewReaderFromFileWithOptions(f, "", nil, pack.ReaderOptions{Limits: pack.ReaderLimits{
		ContainerBytes: uint64(limits.PackBytes),
		FooterBytes:    uint64(limits.FooterBytes),
		Entries:        uint64(limits.PackEntries),
	}})
	if err != nil {
		return nil, mapPackStreamLimit(err)
	}
	keepOpen = true
	return &boundedPackReader{file: f, entries: entries, streamReader: streamReader}, nil
}

func readBoundedPackAt(f *os.File, dst []byte, offset int64, part string) error {
	if _, err := f.ReadAt(dst, offset); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%w: read %s: %w", pack.ErrTruncated, part, err)
		}
		return fmt.Errorf("read pack %s for bounded preflight: %w", part, err)
	}
	return nil
}

func (r *boundedPackReader) readBlob(entry pack.Entry, maxBytes int64) ([]byte, error) {
	limit := uint64(maxBytes)
	if entry.RawLen > limit {
		return nil, newLimitError(LimitBlobRawBytes, entry.RawLen, limit)
	}
	if entry.StoredLen > limit {
		return nil, newLimitError(LimitBlobStoredBytes, entry.StoredLen, limit)
	}
	if entry.RawLen > maxPlatformInt || entry.StoredLen > maxPlatformInt {
		if entry.RawLen > maxPlatformInt {
			return nil, newLimitError(LimitBlobRawBytes, entry.RawLen, maxPlatformInt)
		}
		return nil, newLimitError(LimitBlobStoredBytes, entry.StoredLen, maxPlatformInt)
	}
	stored := make([]byte, int(entry.StoredLen))
	if _, err := r.file.ReadAt(stored, int64(entry.Offset)); err != nil {
		return nil, fmt.Errorf("%w: read stored bytes for %s: %w", pack.ErrCorrupt, entry.ID, err)
	}
	if crc32.Checksum(stored, boundedCRC32CTable) != entry.CRC32C {
		return nil, fmt.Errorf("%w: crc mismatch for %s", pack.ErrCorrupt, entry.ID)
	}
	raw := stored
	if entry.Flags&pack.BlobCompressed != 0 {
		if entry.RawLen == 0 {
			return nil, fmt.Errorf("%w: compressed empty blob", pack.ErrCorrupt)
		}
		var header zstd.Header
		if err := header.Decode(stored); err != nil {
			return nil, fmt.Errorf("%w: decode blob %s frame header: %w", pack.ErrCorrupt, entry.ID, err)
		}
		window := header.WindowSize
		if header.SingleSegment {
			window = header.FrameContentSize
		}
		windowLimit := max(limit, uint64(zstd.MinWindowSize))
		if window > windowLimit {
			return nil, fmt.Errorf("%w: %w", pack.ErrCorrupt,
				newLimitError(LimitBlobWindowBytes, window, windowLimit))
		}
		// Zstd requires at least MinWindowSize bytes of decoder memory even
		// when the authoritative decoded content is smaller. The destination
		// capacity and WithDecodeAllCapLimit still enforce RawLen exactly.
		decoderMemory := max(entry.RawLen, window, uint64(zstd.MinWindowSize))
		decoder, err := zstd.NewReader(nil,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(decoderMemory),
			zstd.WithDecoderMaxWindow(windowLimit),
			zstd.WithDecodeAllCapLimit(true))
		if err != nil {
			return nil, fmt.Errorf("%w: initialize bounded decoder: %w", pack.ErrCorrupt, err)
		}
		raw, err = decoder.DecodeAll(stored, make([]byte, 0, int(entry.RawLen)))
		decoder.Close()
		if err != nil {
			return nil, fmt.Errorf("%w: decode blob %s within declared length: %w", pack.ErrCorrupt, entry.ID, err)
		}
	}
	if uint64(len(raw)) != entry.RawLen {
		return nil, fmt.Errorf("%w: decoded length %d, want %d", pack.ErrCorrupt, len(raw), entry.RawLen)
	}
	if sha256.Sum256(raw) != entry.ID {
		return nil, fmt.Errorf("%w: blob %s", pack.ErrBlobMismatch, entry.ID)
	}
	return raw, nil
}

func (r *boundedPackReader) Close() error {
	if r.streamReader != nil {
		return r.streamReader.Close()
	}
	return r.file.Close()
}
