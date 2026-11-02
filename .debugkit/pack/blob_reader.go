package pack

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"math"
	"os"

	"github.com/klauspost/compress/zstd"
)

// BlobReader streams one plain entry. A terminal io.EOF proves stored CRC,
// decoded length, and raw SHA-256 verification; Close before that point returns
// ErrVerificationIncomplete.
type BlobReader struct {
	parent   *Reader
	ctx      context.Context
	entry    Entry
	stored   *storedStream
	decoded  io.Reader
	decoder  *zstd.Decoder
	hash     hash.Hash
	rawRead  uint64
	terminal error
	verified bool
	closed   bool
}

// BlobReaderOptions applies per-stream codec policy within the Reader's
// constructor limits. Zero fields retain the Reader limit.
type BlobReaderOptions struct {
	WindowBytes uint64
}

type storedStream struct {
	ctx   context.Context
	r     *io.SectionReader
	crc   hash.Hash32
	count uint64
}

func (s *storedStream) Read(p []byte) (int, error) {
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := s.r.Read(p)
	if n > 0 {
		_, _ = s.crc.Write(p[:n])
		s.count += uint64(n)
	}
	return n, err
}

// OpenBlob opens a verified-on-EOF stream for one plain entry. Entry must
// exactly match a value returned by this Reader's Entries method.
func (r *Reader) OpenBlob(ctx context.Context, entry Entry) (*BlobReader, error) {
	return r.OpenBlobWithOptions(ctx, entry, BlobReaderOptions{})
}

// OpenBlobWithOptions opens a verified stream with narrower per-stream limits.
// Entry must exactly match a value returned by this Reader's Entries method.
func (r *Reader) OpenBlobWithOptions(
	ctx context.Context, entry Entry, opts BlobReaderOptions,
) (*BlobReader, error) {
	if ctx == nil {
		return nil, errors.New("pack: nil context")
	}
	entry, err := r.authoritativeEntry(entry)
	if err != nil {
		return nil, err
	}
	if r.enc || entry.Flags&BlobEncrypted != 0 {
		return nil, &UnsupportedStreamError{Feature: StreamEncryptedV1}
	}
	if err := validateStreamingEntry(entry, r.limits); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, os.ErrClosed
	}
	r.streams++
	r.mu.Unlock()
	release := true
	defer func() {
		if release {
			r.releaseStream()
		}
	}()

	section := io.NewSectionReader(r.f, int64(entry.Offset), int64(entry.StoredLen))
	stored := &storedStream{ctx: ctx, r: section, crc: crc32.New(crc32cTable)}
	result := &BlobReader{
		parent: r, ctx: ctx, entry: entry, stored: stored, hash: sha256.New(),
	}
	if entry.Flags&BlobCompressed != 0 {
		window, err := r.streamingWindow(entry)
		if err != nil {
			return nil, err
		}
		windowLimit := r.limits.WindowBytes
		if opts.WindowBytes != 0 && opts.WindowBytes < windowLimit {
			windowLimit = opts.WindowBytes
		}
		if window > windowLimit {
			return nil, &StreamLimitError{Dimension: StreamLimitWindowBytes, Actual: window, Limit: windowLimit}
		}
		decoderMemory := max(entry.RawLen, window, uint64(zstd.MinWindowSize))
		decoder, err := zstd.NewReader(stored,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(decoderMemory),
			zstd.WithDecoderMaxWindow(windowLimit))
		if err != nil {
			return nil, fmt.Errorf("%w: opening zstd stream for blob %s: %w", ErrCorrupt, entry.ID, err)
		}
		result.decoder = decoder
		result.decoded = decoder
	} else {
		result.decoded = stored
	}
	release = false
	return result, nil
}

func (r *Reader) streamingWindow(entry Entry) (uint64, error) {
	headerBytes := make([]byte, min(entry.StoredLen, uint64(zstd.HeaderMaxSize)))
	if _, err := r.f.ReadAt(headerBytes, int64(entry.Offset)); err != nil {
		return 0, fmt.Errorf("%w: reading zstd header for blob %s: %w", ErrCorrupt, entry.ID, err)
	}
	var header zstd.Header
	if err := header.Decode(headerBytes); err != nil {
		return 0, fmt.Errorf("%w: decoding zstd header for blob %s: %w", ErrCorrupt, entry.ID, err)
	}
	if header.HasFCS && header.FrameContentSize != entry.RawLen {
		return 0, fmt.Errorf("%w: zstd content size %d differs from raw length %d for blob %s",
			ErrCorrupt, header.FrameContentSize, entry.RawLen, entry.ID)
	}
	if header.SingleSegment {
		return max(header.FrameContentSize, uint64(zstd.MinWindowSize)), nil
	}
	return header.WindowSize, nil
}

func validateStreamingEntry(entry Entry, limits ReaderLimits) error {
	if entry.Flags & ^(BlobCompressed|BlobEncrypted) != 0 {
		return fmt.Errorf("%w: blob %s has unknown flags %#x", ErrCorrupt, entry.ID, entry.Flags)
	}
	if entry.Offset < MinEntryOffset || entry.Offset > math.MaxInt64 {
		return fmt.Errorf("%w: invalid offset %d for blob %s", ErrCorrupt, entry.Offset, entry.ID)
	}
	if entry.StoredLen > MaxStoredLen || entry.StoredLen > math.MaxInt64 ||
		entry.Offset > math.MaxInt64-entry.StoredLen {
		return fmt.Errorf("%w: invalid stored length %d for blob %s", ErrCorrupt, entry.StoredLen, entry.ID)
	}
	if entry.RawLen > MaxRawLen {
		return fmt.Errorf("%w: invalid raw length %d for blob %s", ErrCorrupt, entry.RawLen, entry.ID)
	}
	if entry.RawLen > limits.RawBytes {
		return &StreamLimitError{Dimension: StreamLimitRawBytes, Actual: entry.RawLen, Limit: limits.RawBytes}
	}
	if entry.StoredLen > limits.StoredBytes {
		return &StreamLimitError{Dimension: StreamLimitStoredBytes, Actual: entry.StoredLen, Limit: limits.StoredBytes}
	}
	if entry.Flags&BlobCompressed == 0 && entry.StoredLen != entry.RawLen {
		return fmt.Errorf("%w: raw frame length %d differs from decoded length %d", ErrCorrupt, entry.StoredLen, entry.RawLen)
	}
	return nil
}

// Read returns decoded bytes. It returns io.EOF only after all terminal
// verification succeeds; an integrity failure may accompany a final prefix.
func (r *BlobReader) Read(p []byte) (int, error) {
	if r.closed {
		if r.terminal != nil {
			return 0, r.terminal
		}
		if r.verified {
			return 0, io.EOF
		}
		return 0, os.ErrClosed
	}
	if r.verified {
		return 0, io.EOF
	}
	if r.terminal != nil {
		return 0, r.terminal
	}
	if len(p) == 0 {
		return 0, nil
	}
	if err := r.ctx.Err(); err != nil {
		return 0, r.fail(err)
	}
	remaining := r.entry.RawLen - r.rawRead
	if remaining == 0 {
		return 0, r.finish()
	}
	want := min(uint64(len(p)), remaining)
	n, readErr := r.decoded.Read(p[:want])
	if n > 0 {
		_, _ = r.hash.Write(p[:n])
		r.rawRead += uint64(n)
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
			return n, r.fail(readErr)
		}
		return n, r.fail(fmt.Errorf("%w: decoding blob %s: %w", ErrCorrupt, r.entry.ID, readErr))
	}
	if errors.Is(readErr, io.EOF) && r.rawRead != r.entry.RawLen {
		return n, r.fail(fmt.Errorf("%w: blob %s decoded to %d bytes, expected %d",
			ErrCorrupt, r.entry.ID, r.rawRead, r.entry.RawLen))
	}
	if r.rawRead == r.entry.RawLen {
		return n, r.finish()
	}
	if n == 0 {
		return 0, r.fail(fmt.Errorf("%w: decoder made no progress for blob %s", ErrCorrupt, r.entry.ID))
	}
	return n, nil
}

func (r *BlobReader) finish() error {
	if r.verified {
		return io.EOF
	}
	if r.terminal != nil {
		return r.terminal
	}
	var probe [1]byte
	n, err := r.decoded.Read(probe[:])
	if n != 0 || err == nil {
		return r.fail(fmt.Errorf("%w: blob %s exceeds raw length %d", ErrCorrupt, r.entry.ID, r.entry.RawLen))
	}
	if !errors.Is(err, io.EOF) {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return r.fail(err)
		}
		return r.fail(fmt.Errorf("%w: finishing blob %s decode: %w", ErrCorrupt, r.entry.ID, err))
	}
	if r.stored.count != r.entry.StoredLen {
		return r.fail(fmt.Errorf("%w: blob %s consumed %d stored bytes, expected %d",
			ErrCorrupt, r.entry.ID, r.stored.count, r.entry.StoredLen))
	}
	if r.stored.crc.Sum32() != r.entry.CRC32C {
		return r.fail(fmt.Errorf("%w: crc mismatch for blob %s", ErrCorrupt, r.entry.ID))
	}
	var got BlobID
	copy(got[:], r.hash.Sum(nil))
	if got != r.entry.ID {
		return r.fail(fmt.Errorf("%w: blob %s", ErrBlobMismatch, r.entry.ID))
	}
	r.verified = true
	return io.EOF
}

func (r *BlobReader) fail(err error) error {
	if r.terminal == nil {
		r.terminal = err
	}
	return r.terminal
}

// Verify drains the stream and returns nil only after verified EOF.
func (r *BlobReader) Verify() error {
	if r.verified {
		return nil
	}
	if r.terminal != nil {
		return r.terminal
	}
	if r.closed {
		return os.ErrClosed
	}
	buf := make([]byte, 64<<10)
	for {
		_, err := r.Read(buf)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// Verified reports whether the stream reached a successfully verified EOF.
func (r *BlobReader) Verified() bool { return r.verified }

// Close releases the stream's descriptor lease. It does not drain implicitly.
func (r *BlobReader) Close() error {
	if r.closed {
		if r.verified {
			return nil
		}
		return errors.Join(r.terminal, ErrVerificationIncomplete)
	}
	r.closed = true
	if r.decoder != nil {
		r.decoder.Close()
	}
	r.parent.releaseStream()
	if r.verified {
		return nil
	}
	return errors.Join(r.terminal, ErrVerificationIncomplete)
}

func (r *Reader) releaseStream() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.streams > 0 {
		r.streams--
	}
}
