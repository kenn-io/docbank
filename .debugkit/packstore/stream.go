package packstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"sync"

	"github.com/klauspost/compress/zstd"
	"go.kenn.io/kit/internal/contextio"
	"go.kenn.io/kit/pack"
)

// VerifiedReadCloser is trusted only after terminal io.EOF, Verify succeeds,
// or Verified returns true. Close does not drain an incomplete stream.
type VerifiedReadCloser interface {
	io.ReadCloser
	Verify() error
	Verified() bool
}

// OpenStream returns catalog-authorized loose or packed content without a
// whole-object allocation. Resolution is retried once if migration removes the
// initially selected physical source. Loose streams preserve Store.Open's size
// availability and are not capped by the maintenance BlobBytes policy; packed
// streams retain configured format and decoder limits.
func (s *Store) OpenStream(ctx context.Context, contentHash Hash) (VerifiedReadCloser, int64, error) {
	if ctx == nil {
		return nil, 0, errors.New("packstore: nil context")
	}
	if err := contentHash.Validate(); err != nil {
		return nil, 0, err
	}
	if s.resolver != nil {
		return resolveBlob(
			ctx,
			s,
			contentHash,
			func(hash Hash) (VerifiedReadCloser, int64, error) {
				return s.openLooseStream(ctx, hash)
			},
			func(hash Hash, entry *IndexEntry) (VerifiedReadCloser, int64, error) {
				return s.openPackedStream(ctx, hash, entry)
			},
		)
	}
	return s.openMultiStream(ctx, contentHash)
}

func (s *Store) openMultiStream(
	ctx context.Context,
	contentHash Hash,
) (VerifiedReadCloser, int64, error) {
	stream, size, location, err := resolveCandidates(
		ctx,
		s,
		contentHash,
		func(backend ReadBackend, location ReadLocation) (VerifiedReadCloser, int64, error) {
			return openBackendStream(ctx, backend, contentHash, location)
		},
	)
	if err != nil {
		return nil, 0, err
	}
	if !s.observeStreams {
		return stream, size, nil
	}
	return &observedVerifiedStream{
		VerifiedReadCloser: stream,
		health:             s.health,
		hash:               contentHash,
		location:           location,
	}, size, nil
}

type observedVerifiedStream struct {
	VerifiedReadCloser
	health   *Health
	hash     Hash
	location ReadLocation
}

func (s *observedVerifiedStream) Read(p []byte) (int, error) {
	n, err := s.VerifiedReadCloser.Read(p)
	s.observe(err)
	return n, err
}

func (s *observedVerifiedStream) Verify() error {
	err := s.VerifiedReadCloser.Verify()
	s.observe(err)
	return err
}

func (s *observedVerifiedStream) Close() error {
	err := s.VerifiedReadCloser.Close()
	s.observe(err)
	return err
}

func (s *observedVerifiedStream) observe(err error) {
	switch {
	case isCandidateFailure(err):
		s.health.Observe(s.hash, s.location, err)
	case err == nil && s.Verified():
		s.health.Clear(s.hash, s.location)
	}
}

// CopyVerified copies catalog-authorized content to caller-owned private
// staging. It neither closes nor syncs dst and succeeds only after verified
// EOF; publication remains a separate caller responsibility.
func (s *Store) CopyVerified(ctx context.Context, contentHash Hash, dst io.Writer) (written int64, resultErr error) {
	if dst == nil {
		return 0, errors.New("packstore: nil verified-copy destination")
	}
	stream, _, err := s.OpenStream(ctx, contentHash)
	if err != nil {
		return 0, err
	}
	defer func() { resultErr = errors.Join(resultErr, stream.Close()) }()
	written, resultErr = io.CopyBuffer(dst, stream, make([]byte, 64<<10))
	return written, resultErr
}

func (s *Store) openPackedStream(
	ctx context.Context, contentHash Hash, indexed *IndexEntry,
) (VerifiedReadCloser, int64, error) {
	return s.openPackedStreamWithPolicy(ctx, contentHash, indexed, true)
}

func (s *Store) openPackedCompatibilityStream(
	ctx context.Context, contentHash Hash, indexed *IndexEntry,
) (VerifiedReadCloser, int64, error) {
	return s.openPackedStreamWithPolicy(ctx, contentHash, indexed, false)
}

func (s *Store) openPackedStreamWithPolicy(
	ctx context.Context,
	contentHash Hash,
	indexed *IndexEntry,
	enforcePolicy bool,
) (VerifiedReadCloser, int64, error) {
	slot, footer, release, err := s.acquirePackedEntry(contentHash, indexed, enforcePolicy)
	if err != nil {
		return nil, 0, err
	}
	if enforcePolicy {
		if err := s.validatePackPolicy(slot); err != nil {
			return nil, 0, errors.Join(err, release())
		}
		limit := uint64(s.limits.BlobBytes)
		if footer.RawLen > limit {
			return nil, 0, errors.Join(newLimitError(LimitBlobRawBytes, footer.RawLen, limit), release())
		}
		if footer.StoredLen > limit {
			return nil, 0, errors.Join(newLimitError(LimitBlobStoredBytes, footer.StoredLen, limit), release())
		}
	}
	var streamOptions pack.BlobReaderOptions
	if enforcePolicy {
		streamOptions.WindowBytes = uint64(max(s.limits.BlobBytes, int64(1<<10)))
	}
	stream, err := slot.reader.OpenBlobWithOptions(ctx, footer, streamOptions)
	if err != nil {
		return nil, 0, errors.Join(mapPackStreamLimit(err), release())
	}
	return &packedVerifiedStream{reader: stream, release: release}, int64(footer.RawLen), nil
}

type packedVerifiedStream struct {
	reader     *pack.BlobReader
	release    func() error
	once       sync.Once
	close      error
	releaseErr error
}

func (s *packedVerifiedStream) Read(p []byte) (int, error) {
	n, err := s.reader.Read(p)
	if err == nil {
		return n, nil
	}
	s.finish()
	if errors.Is(err, io.EOF) && s.close == nil && s.releaseErr == nil {
		return n, io.EOF
	}
	return n, errors.Join(err, s.releaseErr)
}

func (s *packedVerifiedStream) Verify() error {
	err := s.reader.Verify()
	s.finish()
	return errors.Join(err, s.releaseErr)
}

func (s *packedVerifiedStream) Verified() bool { return s.reader.Verified() }

func (s *packedVerifiedStream) Close() error {
	s.finish()
	return errors.Join(s.close, s.releaseErr)
}

func (s *packedVerifiedStream) finish() {
	s.once.Do(func() {
		s.close = s.reader.Close()
		s.releaseErr = s.release()
	})
}

func (s *Store) openLooseStream(ctx context.Context, contentHash Hash) (VerifiedReadCloser, int64, error) {
	object, err := s.openLooseObject(contentHash)
	if err != nil {
		return nil, 0, err
	}
	if object.logicalSize < 0 {
		return nil, 0, errors.Join(
			fmt.Errorf("packstore: negative loose size %d", object.logicalSize),
			object.file.Close(),
		)
	}
	stream, err := newLooseVerifiedStream(ctx, contentHash, object)
	if err != nil {
		return nil, 0, err
	}
	return stream, object.logicalSize, nil
}

func (s *Store) openLooseStreamAt(
	ctx context.Context,
	contentHash Hash,
	location LooseLocation,
) (VerifiedReadCloser, int64, error) {
	object, err := s.openLooseObjectAt(contentHash, location.Encoding)
	if err != nil {
		return nil, 0, err
	}
	if object.logicalSize != location.LogicalSize ||
		object.storedSize != location.StoredSize {
		return nil, 0, errors.Join(
			ErrPhysicalCorrupt,
			fmt.Errorf(
				"packstore: loose authority is logical=%d stored=%d, found logical=%d stored=%d",
				location.LogicalSize,
				location.StoredSize,
				object.logicalSize,
				object.storedSize,
			),
			object.file.Close(),
		)
	}
	stream, err := newLooseVerifiedStream(ctx, contentHash, object)
	if err != nil {
		return nil, 0, err
	}
	return stream, object.logicalSize, nil
}

func newLooseVerifiedStream(
	ctx context.Context,
	contentHash Hash,
	object *looseObject,
) (*looseVerifiedStream, error) {
	return newLooseVerifiedStreamWithDurability(ctx, contentHash, object, false)
}

func newLooseVerifiedStreamWithDurability(
	ctx context.Context,
	contentHash Hash,
	object *looseObject,
	durable bool,
) (*looseVerifiedStream, error) {
	id, err := pack.ParseBlobID(contentHash.String())
	if err != nil {
		return nil, errors.Join(err, object.file.Close())
	}
	stream := &looseVerifiedStream{
		ctx: ctx, object: object, reader: object.file,
		expected: id, size: uint64(object.logicalSize), digest: sha256.New(), durable: durable,
	}
	if object.encoding == LooseEncodingZstd {
		payloadSize := object.storedSize - compressedLooseHeaderSize
		if payloadSize < 0 {
			return nil, errors.Join(
				fmt.Errorf("%w: compressed loose payload size is negative", ErrContentMismatch),
				object.file.Close(),
			)
		}
		stream.payload = &io.LimitedReader{
			R: &contextio.Reader{Context: ctx, Reader: object.file},
			N: payloadSize,
		}
		decoder, decoderErr := newLooseZstdReader(newSingleZstdFrameReader(stream.payload))
		if decoderErr != nil {
			return nil, errors.Join(
				fmt.Errorf("%w: open compressed loose payload: %w", ErrContentMismatch, decoderErr),
				object.file.Close(),
			)
		}
		stream.decoder = decoder
		stream.reader = decoder
	}
	return stream, nil
}

type looseVerifiedStream struct {
	ctx      context.Context
	object   *looseObject
	reader   io.Reader
	decoder  looseZstdReader
	payload  *io.LimitedReader
	expected pack.BlobID
	size     uint64
	digest   hash.Hash
	read     uint64
	terminal error
	closeErr error
	verified bool
	closed   bool
	durable  bool
}

func (s *looseVerifiedStream) Read(p []byte) (int, error) {
	if s.closed {
		if s.terminal != nil {
			return 0, s.terminal
		}
		if s.verified {
			return 0, io.EOF
		}
		return 0, os.ErrClosed
	}
	if len(p) == 0 {
		return 0, nil
	}
	if err := s.ctx.Err(); err != nil {
		return 0, s.fail(err)
	}
	remaining := s.size - s.read
	if remaining == 0 {
		return 0, s.finish()
	}
	want := min(uint64(len(p)), remaining)
	n, readErr := s.reader.Read(p[:want])
	if n > 0 {
		_, _ = s.digest.Write(p[:n])
		s.read += uint64(n)
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		if s.decoder != nil {
			readErr = fmt.Errorf("%w: decode compressed loose payload: %w", ErrContentMismatch, readErr)
		}
		return n, s.fail(readErr)
	}
	if errors.Is(readErr, io.EOF) && s.read != s.size {
		return n, s.fail(fmt.Errorf("%w: loose content ended at %d bytes, expected %d",
			ErrContentMismatch, s.read, s.size))
	}
	if s.read == s.size {
		return n, s.finish()
	}
	if n == 0 {
		return 0, s.fail(io.ErrNoProgress)
	}
	return n, nil
}

func (s *looseVerifiedStream) finish() error {
	if err := s.ctx.Err(); err != nil {
		return s.fail(err)
	}
	var probe [1]byte
	n, err := s.reader.Read(probe[:])
	if n != 0 || err == nil {
		return s.fail(fmt.Errorf(
			"%w: loose decoded size exceeds %d bytes", ErrContentMismatch, s.size,
		))
	}
	if !errors.Is(err, io.EOF) {
		return s.fail(fmt.Errorf("%w: read loose terminal state: %w", ErrContentMismatch, err))
	}
	if s.payload != nil && s.payload.N != 0 {
		return s.fail(fmt.Errorf(
			"%w: compressed loose payload ended with %d unread bytes",
			ErrContentMismatch, s.payload.N,
		))
	}
	var got pack.BlobID
	copy(got[:], s.digest.Sum(nil))
	if got != s.expected {
		return s.fail(fmt.Errorf("%w: loose hash differs from %s", ErrContentMismatch, s.expected))
	}
	if s.durable {
		if err := syncLooseFile(s.object.file); err != nil {
			return s.fail(fmt.Errorf("packstore: sync verified loose content: %w", err))
		}
	}
	if s.payload != nil {
		physical, err := s.object.file.Stat()
		if err != nil {
			return s.fail(fmt.Errorf("packstore: recheck verified compressed loose size: %w", err))
		}
		if physical.Size() != s.object.storedSize {
			return s.fail(fmt.Errorf(
				"%w: %w: compressed loose stored size changed from %d to %d bytes",
				ErrContentMismatch,
				errIdentityChanged,
				s.object.storedSize,
				physical.Size(),
			))
		}
	}
	s.closeErr = s.closePhysical()
	s.closed = true
	if s.closeErr != nil {
		s.terminal = s.closeErr
		return s.terminal
	}
	s.verified = true
	return io.EOF
}

func (s *looseVerifiedStream) fail(err error) error {
	if s.terminal == nil {
		s.closeErr = s.closePhysical()
		s.closed = true
		s.terminal = errors.Join(err, s.closeErr)
	}
	return s.terminal
}

func (s *looseVerifiedStream) Verify() error {
	if s.verified {
		return nil
	}
	if s.terminal != nil {
		return s.terminal
	}
	if s.closed {
		return os.ErrClosed
	}
	buf := make([]byte, 64<<10)
	for {
		_, err := s.Read(buf)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (s *looseVerifiedStream) Verified() bool { return s.verified }

func (s *looseVerifiedStream) Close() error {
	if !s.closed {
		s.closeErr = s.closePhysical()
		s.closed = true
	}
	if s.verified {
		return s.closeErr
	}
	return errors.Join(s.terminal, s.closeErr, pack.ErrVerificationIncomplete)
}

func (s *looseVerifiedStream) closePhysical() error {
	if s.decoder != nil {
		s.decoder.Close()
		s.decoder = nil
	}
	return s.object.file.Close()
}

type zstdFrameReaderState uint8

const (
	zstdFrameHeader zstdFrameReaderState = iota + 1
	zstdFrameBlockHeader
	zstdFrameBlockData
	zstdFrameChecksum
	zstdFrameDone
	zstdFrameInvalid
)

// zstd.HeaderMaxSize is documented as including the first block header, but
// its value does not cover the valid combination of a window descriptor,
// four-byte dictionary ID, and eight-byte frame content size. Allow the four
// additional bytes required by that maximal frame header before the three-byte
// first block header.
const zstdFrameHeaderBufferSize = zstd.HeaderMaxSize + 4

// singleZstdFrameReader exposes exactly one physical zstd frame. The decoder
// therefore reaches EOF at the first frame boundary, leaving any concatenated
// frame or trailing bytes visible in the outer LimitedReader.
type singleZstdFrameReader struct {
	source *io.LimitedReader
	state  zstdFrameReaderState

	header      [zstdFrameHeaderBufferSize]byte
	headerBytes int
	blockHeader [3]byte
	blockBytes  int
	remaining   int64
	lastBlock   bool
	hasChecksum bool
	boundaryErr error
}

func newSingleZstdFrameReader(source *io.LimitedReader) *singleZstdFrameReader {
	return &singleZstdFrameReader{source: source, state: zstdFrameHeader}
}

func (r *singleZstdFrameReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	switch r.state {
	case zstdFrameDone:
		return 0, io.EOF
	case zstdFrameInvalid:
		return 0, r.boundaryErr
	case zstdFrameHeader:
		// Header.Decode includes the first block header in FirstBlock. Discover
		// that boundary one byte at a time so bytes from the block payload are
		// never handed over before remaining has been initialized.
		p = p[:1]
	case zstdFrameBlockHeader:
		p = p[:min(len(p), len(r.blockHeader)-r.blockBytes)]
	case zstdFrameBlockData, zstdFrameChecksum:
		p = p[:min(int64(len(p)), r.remaining)]
	}
	n, err := r.source.Read(p)
	if n > 0 {
		if boundaryErr := r.observe(p[:n]); boundaryErr != nil {
			return n, errors.Join(err, boundaryErr)
		}
	}
	return n, err
}

func (r *singleZstdFrameReader) observe(data []byte) error {
	switch r.state {
	case zstdFrameHeader:
		copy(r.header[r.headerBytes:], data)
		r.headerBytes += len(data)
		var header zstd.Header
		err := header.Decode(r.header[:r.headerBytes])
		if errors.Is(err, io.ErrUnexpectedEOF) || err == nil && !header.Skippable && !header.FirstBlock.OK {
			if r.headerBytes == len(r.header) {
				return r.failBoundary("header exceeds parsing bound")
			}
			if err == nil && header.HeaderSize > 0 && r.headerBytes >= header.HeaderSize+3 {
				return r.failBoundary("invalid first block header")
			}
			return nil
		}
		if err != nil {
			return r.failBoundary(err.Error())
		}
		r.hasChecksum = header.HasCheckSum
		if header.Skippable {
			r.lastBlock = true
			r.remaining = int64(header.SkippableSize)
			r.state = zstdFrameBlockData
			r.finishZstdFramePart()
			return nil
		}
		r.lastBlock = header.FirstBlock.Last
		r.remaining = int64(header.FirstBlock.CompressedSize)
		r.state = zstdFrameBlockData
		r.finishZstdFramePart()
	case zstdFrameBlockHeader:
		copy(r.blockHeader[r.blockBytes:], data)
		r.blockBytes += len(data)
		if r.blockBytes != len(r.blockHeader) {
			return nil
		}
		value := uint32(r.blockHeader[0]) |
			uint32(r.blockHeader[1])<<8 |
			uint32(r.blockHeader[2])<<16
		r.lastBlock = value&1 != 0
		blockType := (value >> 1) & 3
		blockSize := int64(value >> 3)
		switch blockType {
		case 0, 2:
			r.remaining = blockSize
		case 1:
			r.remaining = 1
		default:
			return r.failBoundary("reserved block type")
		}
		r.blockBytes = 0
		r.state = zstdFrameBlockData
		r.finishZstdFramePart()
	case zstdFrameBlockData, zstdFrameChecksum:
		r.remaining -= int64(len(data))
		r.finishZstdFramePart()
	default:
	}
	return nil
}

func (r *singleZstdFrameReader) failBoundary(reason string) error {
	r.state = zstdFrameInvalid
	r.boundaryErr = fmt.Errorf("%w: cannot determine zstd frame boundary: %s", ErrContentMismatch, reason)
	return r.boundaryErr
}

func (r *singleZstdFrameReader) finishZstdFramePart() {
	if r.remaining != 0 {
		return
	}
	switch r.state {
	case zstdFrameBlockData:
		if !r.lastBlock {
			r.state = zstdFrameBlockHeader
			return
		}
		if r.hasChecksum {
			r.state = zstdFrameChecksum
			r.remaining = 4
			return
		}
		r.state = zstdFrameDone
	case zstdFrameChecksum:
		r.state = zstdFrameDone
	default:
	}
}
