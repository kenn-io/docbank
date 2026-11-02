package pack

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ReaderLimits bounds format-controlled quantities before allocation or
// exposure. Zero fields select the corresponding format maximum, except for
// the decoder window's lower ceiling on 32-bit systems.
type ReaderLimits struct {
	ContainerBytes uint64
	FooterBytes    uint64
	Entries        uint64
	RawBytes       uint64
	StoredBytes    uint64

	// WindowBytes limits each streaming decoder's window. Zero allows up to
	// MaxRawLen (4 GiB) on 64-bit systems, or 512 MiB on 32-bit systems. Old
	// single-segment frames can require their full raw size as a window.
	// Set a smaller limit to bound that memory use.
	WindowBytes uint64
}

// ReaderOptions configures bounded pack opening.
type ReaderOptions struct {
	Limits ReaderLimits
}

// ReaderMetadata reports immutable container quantities useful for applying
// caller policy after a format-valid reader is cached.
type ReaderMetadata struct {
	ContainerBytes uint64
	FooterBytes    uint64
	EntryCount     uint64
}

// Reader provides random access to a sealed pack file via pread.
type Reader struct {
	mu       sync.Mutex
	id       string
	f        *os.File
	crypter  *Crypter
	enc      bool
	entries  []Entry
	entrySet map[Entry]struct{}
	limits   ReaderLimits
	metadata ReaderMetadata
	streams  int
	closed   bool
}

// OpenReader opens a sealed pack. The pack ID is the filename minus its
// extension; for encrypted packs it participates in the footer's AAD, so a
// renamed pack fails to open. A crypter passed for a plain pack is ignored.
func OpenReader(path string, crypter *Crypter) (*Reader, error) {
	return OpenReaderWithOptions(path, crypter, ReaderOptions{})
}

// OpenReaderWithOptions opens a sealed pack under explicit allocation and
// format limits.
func OpenReaderWithOptions(path string, crypter *Crypter, opts ReaderOptions) (*Reader, error) {
	base := filepath.Base(path)
	id := strings.TrimSuffix(base, filepath.Ext(base))
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("pack: opening %s: %w", path, err)
	}
	r, err := NewReaderFromFileWithOptions(f, id, crypter, opts)
	if err != nil {
		return nil, fmt.Errorf("pack %s: %w", path, err)
	}
	return r, nil
}

// NewReaderFromFile constructs a sealed-pack reader over an already-open
// descriptor. It takes ownership of f whether construction succeeds or fails.
// id must be the pack ID used for encrypted-footer authentication.
func NewReaderFromFile(f *os.File, id string, crypter *Crypter) (*Reader, error) {
	return NewReaderFromFileWithOptions(f, id, crypter, ReaderOptions{})
}

// NewReaderFromFileWithOptions is NewReaderFromFile with explicit limits.
func NewReaderFromFileWithOptions(f *os.File, id string, crypter *Crypter, opts ReaderOptions) (*Reader, error) {
	if f == nil {
		return nil, errors.New("pack: nil file")
	}
	r, err := newReader(f, id, crypter, normalizeReaderLimits(opts.Limits))
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return r, nil
}

func newReader(f *os.File, id string, crypter *Crypter, limits ReaderLimits) (*Reader, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat: %w", err)
	}
	size := info.Size()
	if size < 0 {
		return nil, fmt.Errorf("negative pack size %d", size)
	}
	if uint64(size) > limits.ContainerBytes {
		return nil, &StreamLimitError{Dimension: StreamLimitContainerBytes, Actual: uint64(size), Limit: limits.ContainerBytes}
	}
	header := make([]byte, headerSize)
	if size < headerSize {
		return nil, ErrTruncated
	}
	if _, err := f.ReadAt(header, 0); err != nil {
		return nil, fmt.Errorf("reading header: %w", err)
	}
	if !bytes.Equal(header[:4], []byte(headerMagic)) {
		return nil, fmt.Errorf("%w: header", ErrBadMagic)
	}
	if header[4] != FormatVersion {
		return nil, fmt.Errorf("%w: version %d", ErrUnsupportedVersion,
			header[4])
	}
	flags := packFlags(header[5])
	if flags&^packEncrypted != 0 {
		return nil, fmt.Errorf("%w: unknown pack flags %#x", ErrCorrupt, header[5])
	}
	enc := flags&packEncrypted != 0
	if enc && crypter == nil {
		return nil, ErrEncrypted
	}

	region, footerStart, err := readFooterRegion(f, size, enc, id, crypter, limits)
	if err != nil {
		return nil, err
	}
	if len(region) >= 4 {
		count := uint64(binary.LittleEndian.Uint32(region[:4]))
		if count > limits.Entries {
			return nil, &StreamLimitError{Dimension: StreamLimitEntryCount, Actual: count, Limit: limits.Entries}
		}
	}
	entries, err := parseFooterRegion(region, footerStart)
	if err != nil {
		return nil, err
	}
	entrySet := make(map[Entry]struct{}, len(entries))
	for _, entry := range entries {
		entrySet[entry] = struct{}{}
		if entry.RawLen > limits.RawBytes {
			return nil, &StreamLimitError{Dimension: StreamLimitRawBytes, Actual: entry.RawLen, Limit: limits.RawBytes}
		}
		if entry.StoredLen > limits.StoredBytes {
			return nil, &StreamLimitError{Dimension: StreamLimitStoredBytes, Actual: entry.StoredLen, Limit: limits.StoredBytes}
		}
	}
	return &Reader{
			id: id, f: f, crypter: crypter, enc: enc, entries: entries, entrySet: entrySet, limits: limits,
			metadata: ReaderMetadata{ContainerBytes: uint64(size), FooterBytes: uint64(len(region)), EntryCount: uint64(len(entries))},
		},
		nil
}

func normalizeReaderLimits(limits ReaderLimits) ReaderLimits {
	set := func(value, maximum uint64) uint64 {
		if value == 0 || value > maximum {
			return maximum
		}
		return value
	}
	limits.ContainerBytes = set(limits.ContainerBytes, math.MaxInt64)
	limits.FooterBytes = set(limits.FooterBytes, MaxFooterLen)
	limits.Entries = set(limits.Entries, MaxFooterLen/entrySize)
	limits.RawBytes = set(limits.RawBytes, MaxRawLen)
	limits.StoredBytes = set(limits.StoredBytes, MaxStoredLen)
	// A single-segment frame declares no window, so its effective window is the
	// whole frame content size. Capping the default at zstd.MaxWindowSize
	// (512 MiB) rejected larger single-segment blobs this package wrote itself.
	// The format ceiling allows those reads at the cost of a potentially
	// blob-sized decoder window. OpenBlobWithOptions enforces explicit lower
	// window limits before creating the decoder.
	limits.WindowBytes = set(limits.WindowBytes, maxDecoderWindow())
	return limits
}

// readFooterRegion locates and reads the footer via two pread calls instead
// of loading a bounded-but-still-huge (up to ~1GiB) tail window: a fixed
// plainTrailerSize-byte read of the very end of the file is always enough to
// hold either trailer form (backup/FORMAT.md, Pack Files) and yields the footer's exact
// offset and length, so the second read fetches only the footer region
// itself. This keeps OpenReader's memory use independent of both pack size
// and footer size for the common case, unlike reading a fixed fraction of
// the file. It returns the decoded (and, for encrypted packs, decrypted)
// footer region and the absolute file offset where that region begins.
func readFooterRegion(f *os.File, size int64, enc bool, id string,
	crypter *Crypter, limits ReaderLimits,
) ([]byte, uint64, error) {
	tailLen := min(size, int64(plainTrailerSize))
	fixedTail := make([]byte, tailLen)
	if _, err := f.ReadAt(fixedTail, size-tailLen); err != nil {
		return nil, 0, fmt.Errorf("reading trailer: %w", err)
	}

	if enc {
		if crypter == nil {
			return nil, 0, ErrEncrypted
		}
		footerOffset, storedLen, err := parseEncryptedTrailer(fixedTail, uint64(size))
		if err != nil {
			return nil, 0, err
		}
		maxStoredFooter := limits.FooterBytes + maxSealOverhead
		if storedLen > maxStoredFooter {
			return nil, 0, &StreamLimitError{Dimension: StreamLimitFooterBytes, Actual: storedLen, Limit: maxStoredFooter}
		}
		buf := make([]byte, storedLen+encTrailerSize)
		if _, err := f.ReadAt(buf, int64(footerOffset)); err != nil {
			return nil, 0, fmt.Errorf("reading footer: %w", err)
		}
		sealed, off, err := extractEncryptedFooterShifted(buf, footerOffset)
		if err != nil {
			return nil, 0, err
		}
		region, err := crypter.OpenObject("pack-footer", id, sealed)
		if err != nil {
			return nil, 0, err
		}
		return region, off, nil
	}

	footerLen, _, err := parsePlainTrailer(fixedTail, uint64(size))
	if err != nil {
		return nil, 0, err
	}
	if uint64(footerLen) > limits.FooterBytes {
		return nil, 0, &StreamLimitError{Dimension: StreamLimitFooterBytes, Actual: uint64(footerLen), Limit: limits.FooterBytes}
	}
	regionStart := uint64(size) - plainTrailerSize - uint64(footerLen)
	buf := make([]byte, uint64(footerLen)+plainTrailerSize)
	if _, err := f.ReadAt(buf, int64(regionStart)); err != nil {
		return nil, 0, fmt.Errorf("reading footer: %w", err)
	}
	region, err := extractPlainFooterRegionShifted(buf, regionStart)
	if err != nil {
		return nil, 0, err
	}
	return region, regionStart, nil
}

// ID returns the pack ULID derived from the filename.
func (r *Reader) ID() string { return r.id }

// Entries returns a copy of the verified footer entries in pack order.
func (r *Reader) Entries() []Entry { return append([]Entry(nil), r.entries...) }

// Metadata returns immutable container quantities from the verified footer.
func (r *Reader) Metadata() ReaderMetadata { return r.metadata }

func (r *Reader) readStored(e Entry) ([]byte, error) {
	e, err := r.authoritativeEntry(e)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, e.StoredLen)

	if _, err := r.f.ReadAt(buf, int64(e.Offset)); err != nil {
		return nil,
			fmt.Errorf("%w: reading stored bytes for %s: %w", ErrCorrupt,
				e.ID, err)
	}
	if crc32.Checksum(buf, crc32cTable) != e.CRC32C {
		return nil, fmt.Errorf("%w: crc mismatch for blob %s", ErrCorrupt,
			e.ID)
	}
	return buf, nil
}

func (r *Reader) authoritativeEntry(entry Entry) (Entry, error) {
	if _, ok := r.entrySet[entry]; !ok {
		return Entry{}, fmt.Errorf(
			"%w: blob %s entry does not match verified footer", ErrCorrupt, entry.ID)
	}
	return entry, nil
}

// VerifyStored checks the stored bytes' CRC without decrypting or decoding.
func (r *Reader) VerifyStored(e Entry) error {
	_, err := r.readStored(e)
	return err
}

// ReadBlob returns the raw content of e, verifying CRC, AEAD, and blob hash.
func (r *Reader) ReadBlob(e Entry) ([]byte, error) {
	stored, err := r.readStored(e)
	if err != nil {
		return nil, err
	}
	if e.Flags&BlobEncrypted != 0 {
		if !r.enc {
			return nil, fmt.Errorf("%w: blob %s flagged encrypted in a plain pack",
				ErrCorrupt, e.ID)
		}
		if r.crypter == nil {
			return nil, ErrEncrypted
		}
		if stored, err = r.crypter.OpenBlob(e.ID, stored); err != nil {
			return nil, err
		}
	}
	raw, err := decodeFrame(stored, e.Flags&BlobCompressed != 0, e.RawLen)
	if err != nil {
		return nil, err
	}
	if ComputeBlobID(raw) != e.ID {
		return nil, fmt.Errorf("%w: blob %s", ErrBlobMismatch, e.ID)
	}
	return raw, nil
}

// Close releases the underlying file. It refuses to race a live BlobReader.
func (r *Reader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	if r.streams != 0 {
		return ErrStreamsActive
	}
	r.closed = true
	return r.f.Close()
}
