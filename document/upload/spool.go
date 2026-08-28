package upload

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sync"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media"
)

const (
	spoolDirectoryPrefix    = ".docbank-upload-"
	spoolFilename           = "source"
	maxProviderMetadataSize = 1 << 20
)

type authorizeStage uint8

const (
	authorizeStageWritten authorizeStage = iota + 1
	authorizeStageReaderOpened
	authorizeStageValidated
)

// Source is one authoritative stream and the application-owned directory in
// which a private, descriptor-held spool may be created. Authorize always
// closes Reader.
type Source struct {
	Reader    io.ReadCloser
	Directory string

	testHook func(authorizeStage, string) error
}

// UploadMetadata contains bounded provider-facing metadata not derived from
// the inspected bytes.
type UploadMetadata struct {
	Filename         string
	ProviderMetadata []byte
}

type authorizedUpload struct {
	mu       sync.Mutex
	reader   *os.File
	metadata document.AuthorizedUploadMetadata
	cleanup  func() error
	stop     func() bool
	closed   bool
	cause    error
}

func (upload *authorizedUpload) Read(buffer []byte) (int, error) {
	upload.mu.Lock()
	defer upload.mu.Unlock()
	if upload.closed || upload.reader == nil {
		if upload.cause != nil {
			return 0, upload.cause
		}
		return 0, os.ErrClosed
	}
	return upload.reader.Read(buffer)
}

// closeCancelled closes the upload and records why. Cancellation can land
// between the context check in Authorize and the caller receiving the upload,
// so the reason has to survive the handoff: without it the caller reads a bare
// os.ErrClosed and cannot tell a cancelled upload from a misused one.
func (upload *authorizedUpload) closeCancelled(cause error) error {
	upload.mu.Lock()
	if !upload.closed && upload.cause == nil {
		upload.cause = cause
	}
	upload.mu.Unlock()
	return upload.Close()
}

func (upload *authorizedUpload) Close() error {
	if upload == nil {
		return nil
	}
	upload.mu.Lock()
	defer upload.mu.Unlock()
	if upload.closed {
		return nil
	}
	upload.closed = true
	var result error
	if upload.stop != nil {
		upload.stop()
		upload.stop = nil
	}
	if upload.reader != nil {
		result = upload.reader.Close()
		upload.reader = nil
	}
	if upload.cleanup != nil {
		result = errors.Join(result, upload.cleanup())
		upload.cleanup = nil
	}
	return result
}

func (upload *authorizedUpload) Metadata() document.AuthorizedUploadMetadata {
	if upload == nil {
		return document.AuthorizedUploadMetadata{}
	}
	return upload.metadata
}

var _ document.AuthorizedUpload = (*authorizedUpload)(nil)

// Authorize copies, syncs, independently reopens, rehashes, and reinspects an
// exact source before returning the core-owned one-shot reader. The named file
// is unlinked or delete-pended before the adapter can receive it.
func Authorize(
	ctx context.Context, source Source, capability media.CapabilityRecord,
	metadata UploadMetadata,
) (authorized document.AuthorizedUpload, retErr error) {
	if source.Reader == nil {
		return nil, errors.New("upload: source reader is required")
	}
	var sourceCloseOnce sync.Once
	var sourceCloseErr error
	closeSource := func() error {
		sourceCloseOnce.Do(func() { sourceCloseErr = source.Reader.Close() })
		return sourceCloseErr
	}
	defer func() {
		if err := closeSource(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("upload: close source: %w", err))
		}
		if retErr != nil && authorized != nil {
			retErr = errors.Join(retErr, authorized.Close())
			authorized = nil
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := media.ValidateCapabilityRecord(capability); err != nil {
		return nil, fmt.Errorf("upload: invalid capability record: %w", err)
	}
	if !capability.Eligible {
		return nil, fmt.Errorf("upload: capability record is ineligible: %s", capability.Reason)
	}
	policy, local := capability.InspectionPolicy()
	if !local {
		return nil, errors.New("upload: capability record lacks local inspection authority")
	}
	if metadata.Filename != policy.Filename {
		return nil, errors.New("upload: filename does not match capability record")
	}
	if len(metadata.ProviderMetadata) > maxProviderMetadataSize {
		return nil, errors.New("upload: provider metadata exceeds byte limit")
	}
	if source.Directory == "" || !filepath.IsAbs(source.Directory) {
		return nil, errors.New("upload: spool directory must be an absolute path")
	}

	directory, err := openSpoolDirectory(source.Directory)
	if err != nil {
		return nil, fmt.Errorf("upload: create private spool: %w", err)
	}
	cleanupDirectory := true
	defer func() {
		if cleanupDirectory {
			retErr = errors.Join(retErr, directory.cleanup())
		}
	}()
	writer, err := directory.create(spoolFilename)
	if err != nil {
		return nil, fmt.Errorf("upload: create exclusive spool: %w", err)
	}
	writerOpen := true
	defer func() {
		if writerOpen {
			retErr = errors.Join(retErr, writer.Close())
		}
	}()

	stopClose := context.AfterFunc(ctx, func() { _ = closeSource() })
	hasher := sha256.New()
	written, err := copyContext(ctx, io.MultiWriter(writer, hasher),
		io.LimitReader(source.Reader, capability.SourceBytes+1))
	stopClose()
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, fmt.Errorf("upload: copy source: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if written != capability.SourceBytes || hex.EncodeToString(hasher.Sum(nil)) != capability.SourceSHA256 {
		return nil, errors.New("upload: copied source does not match capability record")
	}
	if err := writer.Sync(); err != nil {
		return nil, fmt.Errorf("upload: sync spool: %w", err)
	}
	writtenInfo, err := writer.Stat()
	if err != nil {
		return nil, fmt.Errorf("upload: stat writer: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("upload: close writer: %w", err)
	}
	writerOpen = false
	if err := directory.sync(); err != nil {
		return nil, fmt.Errorf("upload: sync spool directory: %w", err)
	}
	if err := callTestHook(source.testHook, authorizeStageWritten, directory.path(spoolFilename)); err != nil {
		return nil, err
	}

	reader, err := directory.openReader(spoolFilename, writtenInfo)
	if err != nil {
		return nil, fmt.Errorf("upload: open independent spool reader: %w", err)
	}
	readerOpen := true
	defer func() {
		if readerOpen {
			retErr = errors.Join(retErr, reader.Close())
		}
	}()
	if err := callTestHook(source.testHook, authorizeStageReaderOpened, directory.path(spoolFilename)); err != nil {
		return nil, err
	}
	if err := directory.unlink(spoolFilename); err != nil {
		return nil, fmt.Errorf("upload: unlink spool before independent validation: %w", err)
	}
	if err := directory.sync(); err != nil {
		return nil, fmt.Errorf("upload: sync spool unlink: %w", err)
	}
	secondHasher := sha256.New()
	secondSize, err := io.Copy(secondHasher, reader)
	if err != nil {
		return nil, fmt.Errorf("upload: independently hash spool: %w", err)
	}
	if secondSize != capability.SourceBytes ||
		hex.EncodeToString(secondHasher.Sum(nil)) != capability.SourceSHA256 {
		return nil, errors.New("upload: independent spool hash does not match authorized source")
	}
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("upload: rewind spool for capability validation: %w", err)
	}
	reinspected, err := media.InspectCapability(reader, policy)
	if err != nil {
		return nil, fmt.Errorf("upload: inspect sealed spool: %w", err)
	}
	if !reflect.DeepEqual(reinspected, capability) {
		return nil, errors.New("upload: sealed spool capability does not match authorization")
	}
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("upload: rewind authorized spool: %w", err)
	}
	if err := callTestHook(source.testHook, authorizeStageValidated, directory.path(spoolFilename)); err != nil {
		return nil, err
	}
	providerDigest := sha256.Sum256(metadata.ProviderMetadata)
	result := &authorizedUpload{reader: reader, metadata: document.AuthorizedUploadMetadata{
		Filename: metadata.Filename, MediaFamily: capability.MediaFamily,
		MediaType: capability.MediaType, ByteLength: capability.SourceBytes,
		SHA256: capability.SourceSHA256, CapabilityRecordChecksum: capability.Checksum,
		ProviderMetadataChecksum: hex.EncodeToString(providerDigest[:]), InputKind: capability.InputKind,
	}}
	result.cleanup = directory.cleanup
	readerOpen = false
	cleanupDirectory = false
	result.mu.Lock()
	result.stop = context.AfterFunc(ctx, func() { _ = result.closeCancelled(context.Cause(ctx)) })
	contextErr := ctx.Err()
	result.mu.Unlock()
	if contextErr != nil {
		_ = result.closeCancelled(context.Cause(ctx))
		return nil, contextErr
	}
	authorized = result
	return authorized, nil
}

// RecoverStale removes only package-owned private spool directories. It is
// safe to call at startup after prior process crashes.
func RecoverStale(ctx context.Context, base string) (int, error) {
	if base == "" || !filepath.IsAbs(base) {
		return 0, errors.New("upload: stale spool root must be an absolute path")
	}
	root, err := openStableRoot(base)
	if err != nil {
		return 0, fmt.Errorf("upload: open stale spool root: %w", err)
	}
	defer func() { _ = root.Close() }()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return 0, fmt.Errorf("upload: read stale spool root: %w", err)
	}
	removed := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if !stringsHasPrefix(entry.Name(), spoolDirectoryPrefix) {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			continue
		}
		if err := root.RemoveAll(entry.Name()); err != nil {
			return removed, fmt.Errorf("upload: remove stale spool %s: %w", entry.Name(), err)
		}
		removed++
	}
	return removed, nil
}

func openStableRoot(path string) (*os.Root, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, errors.New("upload: spool root is a symlink, reparse point, or non-directory")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	after, err := root.Stat(".")
	if err != nil || !after.IsDir() || !os.SameFile(before, after) {
		_ = root.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("upload: spool root identity changed while opening")
	}
	return root, nil
}

func copyContext(ctx context.Context, writer io.Writer, reader io.Reader) (int64, error) {
	buffer := make([]byte, 128<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		count, readErr := reader.Read(buffer)
		if count > 0 {
			written, writeErr := writer.Write(buffer[:count])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != count {
				return total, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return total, nil
			}
			return total, readErr
		}
	}
}

func callTestHook(hook func(authorizeStage, string) error, stage authorizeStage, path string) error {
	if hook == nil {
		return nil
	}
	return hook(stage, path)
}

func randomSpoolName() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("read spool name entropy: %w", err)
	}
	return spoolDirectoryPrefix + hex.EncodeToString(value[:]), nil
}

func stringsHasPrefix(value, prefix string) bool {
	return len(value) >= len(prefix) && value[:len(prefix)] == prefix
}
