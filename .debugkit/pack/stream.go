package pack

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/klauspost/compress/zstd"
)

var (
	// ErrStreamUnsupported reports a format-v1 feature that cannot preserve its
	// security contract while streaming.
	ErrStreamUnsupported = errors.New("pack: streaming unsupported")
	// ErrStreamLimit reports a configured streaming work limit.
	ErrStreamLimit = errors.New("pack: streaming limit exceeded")
	// ErrVerificationIncomplete reports a stream closed before verified EOF.
	ErrVerificationIncomplete = errors.New("pack: verification incomplete")
	// ErrStreamsActive reports an attempt to close a Reader with live streams.
	ErrStreamsActive = errors.New("pack: blob streams active")
)

// StreamFeature identifies an unsupported streaming feature.
type StreamFeature string

const (
	// StreamEncryptedV1 identifies format-v1 whole-frame authenticated encryption.
	StreamEncryptedV1 StreamFeature = "encrypted_v1"
)

// UnsupportedStreamError carries the unsupported feature.
type UnsupportedStreamError struct {
	Feature StreamFeature
}

func (e *UnsupportedStreamError) Error() string {
	return fmt.Sprintf("%s: %s", ErrStreamUnsupported, e.Feature)
}

func (e *UnsupportedStreamError) Unwrap() error { return ErrStreamUnsupported }

// StreamLimitDimension identifies a bounded streaming quantity.
type StreamLimitDimension string

const (
	StreamLimitRawBytes       StreamLimitDimension = "raw_bytes"
	StreamLimitStoredBytes    StreamLimitDimension = "stored_bytes"
	StreamLimitContainerBytes StreamLimitDimension = "container_bytes"
	StreamLimitFooterBytes    StreamLimitDimension = "footer_bytes"
	StreamLimitEntryCount     StreamLimitDimension = "entry_count"
	StreamLimitScratchBytes   StreamLimitDimension = "scratch_bytes"
	StreamLimitWindowBytes    StreamLimitDimension = "window_bytes"
)

// StreamLimitError carries the bounded quantity and configured ceiling.
type StreamLimitError struct {
	Dimension StreamLimitDimension
	Actual    uint64
	Limit     uint64
}

func (e *StreamLimitError) Error() string {
	return fmt.Sprintf("%s: %s is %d, limit %d", ErrStreamLimit, e.Dimension, e.Actual, e.Limit)
}

func (e *StreamLimitError) Unwrap() error { return ErrStreamLimit }

// AppendStreamOptions configures preparation of one streamed blob.
type AppendStreamOptions struct {
	// ExpectedID, when non-nil, must equal the streamed content identity.
	ExpectedID *BlobID
	// ScratchDir receives exact-owned temporary frames. The system temporary
	// directory is used by PrepareBlob when this is empty; Writer.AppendStream
	// instead defaults it to the writer's staging directory.
	ScratchDir string
	// ScratchBytes bounds peak scratch allocation. Zero selects the format
	// maximum; automatic higher-level work should always supply a finite limit.
	ScratchBytes uint64
}

// PreparedBlob owns one complete plain frame in private scratch storage.
// Close discards a value that will not be passed to Writer.AppendPrepared.
type PreparedBlob struct {
	mu          sync.Mutex
	f           *os.File
	path        string
	info        os.FileInfo
	id          BlobID
	rawLen      uint64
	storedLen   uint64
	scratchPeak uint64
	crc         uint32
	compressed  bool
	consumed    bool
	err         error
}

func (p *PreparedBlob) take() (*os.File, string, os.FileInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.consumed {
		if p.err != nil {
			return nil, "", nil, p.err
		}
		return nil, "", nil, errors.New("pack: prepared blob already consumed")
	}
	p.consumed = true
	f, path, info := p.f, p.path, p.info
	p.f = nil
	return f, path, info, nil
}

func (p *PreparedBlob) finish(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
}

// ID returns the verified raw content identity.
func (p *PreparedBlob) ID() BlobID { return p.id }

// RawLen returns the verified raw byte count.
func (p *PreparedBlob) RawLen() uint64 { return p.rawLen }

// StoredLen returns the chosen frame byte count.
func (p *PreparedBlob) StoredLen() uint64 { return p.storedLen }

// ScratchBytes returns the peak scratch bytes used while preparing this blob.
func (p *PreparedBlob) ScratchBytes() uint64 { return p.scratchPeak }

// Close removes exact scratch owned by p. It is idempotent.
func (p *PreparedBlob) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.consumed {
		return p.err
	}
	p.consumed = true
	p.err = closeAndRemoveScratch(p.f, p.path)
	p.f = nil
	return p.err
}

// PrepareBlob reads exactly rawLen bytes, verifies their identity, and
// trial-compresses them into bounded private scratch. It never closes src.
func PrepareBlob(
	ctx context.Context,
	src io.Reader,
	rawLen uint64,
	zstdLevel int,
	opts AppendStreamOptions,
) (_ *PreparedBlob, resultErr error) {
	if ctx == nil {
		return nil, errors.New("pack: nil context")
	}
	if src == nil {
		return nil, errors.New("pack: nil stream source")
	}
	if rawLen > MaxRawLen {
		return nil, &StreamLimitError{Dimension: StreamLimitRawBytes, Actual: rawLen, Limit: MaxRawLen}
	}
	peak, ok := preparationScratchBound(rawLen)
	if !ok {
		return nil, &StreamLimitError{Dimension: StreamLimitScratchBytes, Actual: ^uint64(0), Limit: opts.ScratchBytes}
	}
	if opts.ScratchBytes > 0 && peak > opts.ScratchBytes {
		return nil, &StreamLimitError{Dimension: StreamLimitScratchBytes, Actual: peak, Limit: opts.ScratchBytes}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	dir := opts.ScratchDir
	if dir == "" {
		dir = os.TempDir()
	}
	rawFile, err := os.CreateTemp(dir, "pack-prepared-*.raw.staging")
	if err != nil {
		return nil, fmt.Errorf("pack: creating raw scratch: %w", err)
	}
	rawPath := rawFile.Name()
	defer func() {
		if rawFile != nil {
			resultErr = errors.Join(resultErr, closeAndRemoveScratch(rawFile, rawPath))
		}
	}()

	var compressedFile *os.File
	var compressedPath string
	if rawLen >= zstd.MinWindowSize {
		compressedFile, err = os.CreateTemp(dir, "pack-prepared-*.zstd.staging")
		if err != nil {
			return nil, fmt.Errorf("pack: creating compressed scratch: %w", err)
		}
		compressedPath = compressedFile.Name()
		defer func() {
			if compressedFile != nil {
				resultErr = errors.Join(resultErr, closeAndRemoveScratch(compressedFile, compressedPath))
			}
		}()
	}

	rawCRC := crc32.New(crc32cTable)
	rawHash := sha256.New()
	writers := []io.Writer{rawFile, rawCRC, rawHash}
	compressedCRC := crc32.New(crc32cTable)
	var encoder *zstd.Encoder
	if compressedFile != nil {
		encoder, err = zstd.NewWriter(io.MultiWriter(compressedFile, compressedCRC),
			zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(normalizeZstdLevel(zstdLevel))),
			zstd.WithEncoderConcurrency(1),
			zstd.WithWindowSize(streamWindowSize(rawLen)))
		if err != nil {
			return nil, fmt.Errorf("pack: creating streaming zstd encoder: %w", err)
		}
		encoder.ResetContentSize(io.MultiWriter(compressedFile, compressedCRC), int64(rawLen))
		writers = append(writers, encoder)
	}

	readErr := copyDeclared(ctx, io.MultiWriter(writers...), src, rawLen)
	if encoder != nil {
		readErr = errors.Join(readErr, encoder.Close())
	}
	if readErr != nil {
		return nil, readErr
	}

	id := BlobID(rawHash.Sum(nil))
	if opts.ExpectedID != nil && id != *opts.ExpectedID {
		return nil, fmt.Errorf("%w: streamed blob is %s, expected %s", ErrBlobMismatch, id, *opts.ExpectedID)
	}
	rawInfo, err := rawFile.Stat()
	if err != nil {
		return nil, fmt.Errorf("pack: stat raw scratch: %w", err)
	}
	if rawInfo.Size() < 0 || uint64(rawInfo.Size()) != rawLen {
		return nil, fmt.Errorf("pack: raw scratch length changed: got %d, want %d", rawInfo.Size(), rawLen)
	}

	chosenFile, chosenPath, chosenInfo := rawFile, rawPath, rawInfo
	chosenCRC := rawCRC.Sum32()
	storedLen := rawLen
	compressed := false
	actualPeak := rawLen
	if compressedFile != nil {
		compressedInfo, statErr := compressedFile.Stat()
		if statErr != nil {
			return nil, fmt.Errorf("pack: stat compressed scratch: %w", statErr)
		}
		if compressedInfo.Size() < 0 {
			return nil, fmt.Errorf("pack: negative compressed scratch length %d", compressedInfo.Size())
		}
		compressedLen := uint64(compressedInfo.Size())
		actualPeak += compressedLen
		if opts.ScratchBytes > 0 && actualPeak > opts.ScratchBytes {
			return nil, &StreamLimitError{Dimension: StreamLimitScratchBytes, Actual: actualPeak, Limit: opts.ScratchBytes}
		}
		minSavings := minCompressionSavings64(rawLen)
		if compressedLen <= rawLen-minSavings {
			chosenFile, chosenPath, chosenInfo = compressedFile, compressedPath, compressedInfo
			chosenCRC = compressedCRC.Sum32()
			storedLen = compressedLen
			compressed = true
			if err := closeAndRemoveScratch(rawFile, rawPath); err != nil {
				return nil, err
			}
			rawFile = nil
		} else {
			if err := closeAndRemoveScratch(compressedFile, compressedPath); err != nil {
				return nil, err
			}
			compressedFile = nil
		}
	}

	if _, err := chosenFile.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("pack: rewinding prepared frame: %w", err)
	}
	if compressed {
		compressedFile = nil
	} else {
		rawFile = nil
	}
	return &PreparedBlob{
		f: chosenFile, path: chosenPath, info: chosenInfo, id: id,
		rawLen: rawLen, storedLen: storedLen, scratchPeak: actualPeak,
		crc: chosenCRC, compressed: compressed,
	}, nil
}

func preparationScratchBound(rawLen uint64) (uint64, bool) {
	if rawLen < zstd.MinWindowSize {
		return rawLen, true
	}
	compressed := rawLen + (rawLen >> 8) + maxZstdFrameOverhead
	if compressed < rawLen {
		return 0, false
	}
	peak := rawLen + compressed
	return peak, peak >= rawLen
}

func minCompressionSavings64(rawLen uint64) uint64 {
	return max(uint64(1), (rawLen*3+99)/100)
}

func normalizeZstdLevel(level int) int {
	if level <= 0 {
		return DefaultZstdLevel
	}
	return level
}

func streamWindowSize(rawLen uint64) int {
	window := uint64(zstd.MinWindowSize)
	for window < streamMaxWindowSize && window<<1 <= rawLen {
		window <<= 1
	}
	return int(window)
}

const streamMaxWindowSize = 8 << 20

func copyDeclared(ctx context.Context, dst io.Writer, src io.Reader, rawLen uint64) error {
	written, err := copyContext(ctx, dst, io.LimitReader(src, int64(rawLen)), rawLen)
	if err != nil {
		return fmt.Errorf("pack: reading stream source after %d bytes: %w", written, err)
	}
	if written != rawLen {
		return fmt.Errorf("%w: stream source ended at %d bytes, expected %d", ErrTruncated, written, rawLen)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var probe [1]byte
	n, err := src.Read(probe[:])
	if n != 0 || err == nil {
		return fmt.Errorf("%w: stream source exceeds declared length %d", ErrCorrupt, rawLen)
	}
	if !errors.Is(err, io.EOF) {
		return fmt.Errorf("pack: probing stream source length: %w", err)
	}
	return nil
}

func copyContext(ctx context.Context, dst io.Writer, src io.Reader, limit uint64) (uint64, error) {
	buf := make([]byte, 64<<10)
	var total uint64
	for total < limit {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		want := min(uint64(len(buf)), limit-total)
		n, readErr := src.Read(buf[:want])
		if n > 0 {
			wn, writeErr := dst.Write(buf[:n])
			total += uint64(wn)
			if writeErr != nil {
				return total, writeErr
			}
			if wn != n {
				return total, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return total, nil
			}
			return total, readErr
		}
		if n == 0 {
			return total, io.ErrNoProgress
		}
	}
	return total, nil
}

func closeAndRemoveScratch(f *os.File, path string) error {
	var err error
	if f != nil {
		err = f.Close()
	}
	if path != "" {
		removeErr := os.Remove(filepath.Clean(path))
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		err = errors.Join(err, removeErr)
	}
	if err != nil {
		return fmt.Errorf("pack: cleaning scratch %s: %w", filepath.Base(path), err)
	}
	return nil
}
