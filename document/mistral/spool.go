package mistral

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

const (
	spoolFilenamePrefix    = ".mistral-ocr-"
	spoolReservationFile   = ".mistral-ocr-reservations.lock"
	spoolLockRetryInterval = 50 * time.Millisecond
)

// ErrSpoolCapacity marks a retryable quota or free-space refusal.
var ErrSpoolCapacity = errors.New("mistral OCR spool capacity unavailable")

// PrepareOptions supplies application-owned staging bounds and expected source
// metadata. Policy.MaxDocumentBytes remains the file-byte limit.
type PrepareOptions struct {
	Directory         string
	DeclaredMediaType string
	ExpectedSize      int64
	ExpectedSHA256    string
	MaxSpoolBytes     int64
	MinFreeBytes      int64
}

// PreparedDocument is an immutable private staging file. Its filesystem path
// is never exposed.
type PreparedDocument struct {
	mu         sync.Mutex
	path       string
	format     CandidateFormat
	size       int64
	sha256     string
	mediaType  string
	localUnits int
	released   bool
}

// Format returns the locally detected format.
func (d *PreparedDocument) Format() CandidateFormat {
	if d == nil {
		return CandidateFormat{}
	}
	return d.format
}

// Size returns the verified byte count.
func (d *PreparedDocument) Size() int64 {
	if d == nil {
		return 0
	}
	return d.size
}

// SHA256 returns the verified lowercase content digest.
func (d *PreparedDocument) SHA256() string {
	if d == nil {
		return ""
	}
	return d.sha256
}

// MediaType returns the detected canonical media type.
func (d *PreparedDocument) MediaType() string {
	if d == nil {
		return ""
	}
	return d.mediaType
}

// Release removes only this package-created staging file. It is idempotent and
// makes the document unavailable to future Process calls.
func (d *PreparedDocument) Release() error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.released = true
	if d.path == "" {
		return nil
	}
	releaseLock, err := acquireSpoolReservationLock(context.Background(), filepath.Dir(d.path))
	if err != nil {
		return err
	}
	defer releaseLock()
	if err := os.Remove(d.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove Mistral OCR spool %s: %w", filepath.Base(d.path), err)
	}
	d.path = ""
	return nil
}

type preparedSnapshot struct {
	path       string
	format     CandidateFormat
	size       int64
	sha256     string
	mediaType  string
	localUnits int
}

func (d *PreparedDocument) snapshot() (preparedSnapshot, error) {
	if d == nil {
		return preparedSnapshot{}, errors.New("mistral OCR prepared document is nil")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.released || d.path == "" {
		return preparedSnapshot{}, errors.New("mistral OCR prepared document was released")
	}
	return preparedSnapshot{
		path: d.path, format: d.format, size: d.size, sha256: d.sha256,
		mediaType: d.mediaType, localUnits: d.localUnits,
	}, nil
}

// Prepare copies one authoritative stream into a verified private staging
// file. It always closes source.
func Prepare(
	ctx context.Context,
	source io.ReadCloser,
	policy Policy,
	options PrepareOptions,
) (_ *PreparedDocument, err error) {
	if source == nil {
		return nil, errors.New("mistral OCR staging requires a source")
	}
	sourceClosed := false
	defer func() {
		if sourceClosed {
			return
		}
		if closeErr := source.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close Mistral OCR source: %w", closeErr)
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if policy.digest == "" {
		return nil, errors.New("mistral policy is invalid; use NewPolicy")
	}
	maxDocumentBytes := policy.values.MaxDocumentBytes
	if options.Directory == "" || options.ExpectedSize < 0 ||
		options.ExpectedSize > maxDocumentBytes || options.MaxSpoolBytes < maxDocumentBytes ||
		options.MinFreeBytes <= 0 {
		return nil, errors.New("mistral OCR staging has invalid bounds")
	}
	if len(options.ExpectedSHA256) != sha256.Size*2 ||
		options.ExpectedSHA256 != strings.ToLower(options.ExpectedSHA256) {
		return nil, errors.New("mistral OCR staging requires a lowercase SHA-256")
	}
	if _, decodeErr := hex.DecodeString(options.ExpectedSHA256); decodeErr != nil {
		return nil, errors.New("mistral OCR staging requires a lowercase SHA-256")
	}
	if err := validatePrivateDirectory(options.Directory); err != nil {
		return nil, err
	}

	releaseLock, err := acquireSpoolReservationLock(ctx, options.Directory)
	if err != nil {
		return nil, err
	}
	defer releaseLock()
	if capacityErr := checkSpoolCapacity(options, maxDocumentBytes); capacityErr != nil {
		return nil, capacityErr
	}

	file, err := os.CreateTemp(options.Directory, spoolFilenamePrefix+"*")
	if err != nil {
		return nil, wrapSpoolIOError("create Mistral OCR spool", err)
	}
	path := file.Name()
	success := false
	fileOpen := true
	defer func() {
		if success {
			return
		}
		var cleanupErr error
		if fileOpen {
			if closeErr := file.Close(); closeErr != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("close partial Mistral OCR spool: %w", closeErr))
			}
		}
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf(
				"remove partial Mistral OCR spool %s: %w", filepath.Base(path), removeErr,
			))
		}
		if cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
	}()
	if err := secureCreatedFile(file); err != nil {
		return nil, wrapSpoolIOError("secure Mistral OCR spool", err)
	}

	hash := sha256.New()
	contextSource := &contextReader{ctx: ctx, reader: source}
	limited := io.LimitReader(contextSource, options.ExpectedSize)
	written, err := io.Copy(io.MultiWriter(file, hash), limited)
	if err != nil {
		closeSourceErr := source.Close()
		sourceClosed = true
		return nil, wrapSpoolIOError("copy Mistral OCR spool", errors.Join(err, closeSourceErr))
	}
	var extra [1]byte
	extraBytes, extraErr := io.ReadFull(contextSource, extra[:])
	closeSourceErr := source.Close()
	sourceClosed = true
	if extraErr != nil && !errors.Is(extraErr, io.EOF) {
		return nil, fmt.Errorf("verify Mistral OCR source length: %w", extraErr)
	}
	if closeSourceErr != nil {
		return nil, fmt.Errorf("close Mistral OCR source: %w", closeSourceErr)
	}
	if written != options.ExpectedSize || extraBytes != 0 {
		return nil, errors.New("mistral OCR source size mismatch")
	}
	actualHash := hex.EncodeToString(hash.Sum(nil))
	if actualHash != options.ExpectedSHA256 {
		return nil, errors.New("mistral OCR source hash mismatch")
	}
	if err := file.Sync(); err != nil {
		return nil, wrapSpoolIOError("sync Mistral OCR spool", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind Mistral OCR spool: %w", err)
	}
	format, err := DetectFormat(file, written, options.DeclaredMediaType)
	if err != nil {
		return nil, err
	}
	localUnits, err := countLocalUnits(format, file, written)
	if err != nil {
		return nil, fmt.Errorf("count local Mistral OCR units: %w", err)
	}
	closeErr := file.Close()
	fileOpen = false
	if closeErr != nil {
		return nil, wrapSpoolIOError("close Mistral OCR spool", closeErr)
	}

	success = true
	return &PreparedDocument{
		path: path, format: format, size: written, sha256: actualHash,
		mediaType: format.MediaType, localUnits: localUnits,
	}, nil
}

func wrapSpoolIOError(operation string, err error) error {
	operationError := fmt.Errorf("%s: %w", operation, err)
	if isSpoolCapacityError(err) {
		return fmt.Errorf("%w: %w", ErrSpoolCapacity, operationError)
	}
	return operationError
}

func countLocalUnits(format CandidateFormat, reader io.ReaderAt, size int64) (int, error) {
	counter := localUnitCounters[format.ID]
	if counter == nil {
		return 0, nil
	}
	return counter(reader, size)
}

var localUnitCounters = map[string]func(io.ReaderAt, int64) (int, error){}

// ScavengeSpoolDirectory removes stale package-created regular files. It
// leaves unrelated regular files and fails closed on unsafe entries.
func ScavengeSpoolDirectory(directory string, staleBefore time.Time) (int, error) {
	if directory == "" || staleBefore.IsZero() {
		return 0, errors.New("mistral OCR spool scavenging requires a directory and cutoff")
	}
	if err := validatePrivateDirectory(directory); err != nil {
		return 0, err
	}
	release, err := acquireSpoolReservationLock(context.Background(), directory)
	if err != nil {
		return 0, err
	}
	defer release()
	entries, err := os.ReadDir(directory)
	if err != nil {
		return 0, fmt.Errorf("read Mistral OCR spool directory: %w", err)
	}
	stale := make([]string, 0, len(entries))
	for _, entry := range entries {
		path := filepath.Join(directory, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return 0, fmt.Errorf("inspect Mistral OCR spool entry: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return 0, errors.New("mistral OCR spool directory contains an unsafe entry")
		}
		if entry.Name() == spoolReservationFile {
			continue
		}
		if !strings.HasPrefix(entry.Name(), spoolFilenamePrefix) || !info.ModTime().Before(staleBefore) {
			continue
		}
		stale = append(stale, path)
	}
	removed := 0
	for _, path := range stale {
		if err := os.Remove(path); err != nil {
			return removed, fmt.Errorf("remove stale Mistral OCR spool %s: %w", filepath.Base(path), err)
		}
		removed++
	}
	return removed, nil
}

func checkSpoolCapacity(options PrepareOptions, maxDocumentBytes int64) error {
	entries, err := os.ReadDir(options.Directory)
	if err != nil {
		return fmt.Errorf("read Mistral OCR spool usage: %w", err)
	}
	var used int64
	for _, entry := range entries {
		info, err := os.Lstat(filepath.Join(options.Directory, entry.Name()))
		if err != nil {
			return fmt.Errorf("inspect Mistral OCR spool usage: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() < 0 {
			return errors.New("mistral OCR spool directory contains an unsafe entry")
		}
		if used > options.MaxSpoolBytes-info.Size() {
			return fmt.Errorf("%w: mistral OCR spool quota is exhausted", ErrSpoolCapacity)
		}
		used += info.Size()
	}
	if used > options.MaxSpoolBytes-options.ExpectedSize || options.ExpectedSize > maxDocumentBytes {
		return fmt.Errorf("%w: mistral OCR spool quota is exhausted", ErrSpoolCapacity)
	}
	available, err := availableDiskBytes(options.Directory)
	if err != nil {
		return fmt.Errorf("inspect Mistral OCR spool free space: %w", err)
	}
	if available < options.ExpectedSize || available-options.ExpectedSize < options.MinFreeBytes {
		return fmt.Errorf("%w: mistral OCR spool free-space reserve would be crossed", ErrSpoolCapacity)
	}
	return nil
}

func acquireSpoolReservationLock(ctx context.Context, directory string) (func(), error) {
	lockPath := filepath.Join(directory, spoolReservationFile)
	lock := flock.New(lockPath, flock.SetPermissions(0o600))
	locked, err := lock.TryLockContext(ctx, spoolLockRetryInterval)
	if err != nil {
		return nil, fmt.Errorf("acquire Mistral OCR spool reservation lock: %w", err)
	}
	if !locked {
		return nil, errors.New("mistral OCR spool reservation lock was not acquired")
	}
	lockInfo, err := lock.Stat()
	if err != nil {
		_ = lock.Unlock()
		return nil, fmt.Errorf("inspect Mistral OCR spool reservation lock: %w", err)
	}
	verified, err := openPrivateFile(lockPath)
	if err != nil {
		_ = lock.Unlock()
		return nil, fmt.Errorf("verify Mistral OCR spool reservation lock: %w", err)
	}
	verifiedInfo, statErr := verified.Stat()
	closeErr := verified.Close()
	if statErr != nil || closeErr != nil || !os.SameFile(lockInfo, verifiedInfo) {
		_ = lock.Unlock()
		return nil, errors.New("mistral OCR spool reservation lock changed while opening")
	}
	return func() { _ = lock.Unlock() }, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}
