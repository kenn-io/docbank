package gcs

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/url"
	"strings"

	"cloud.google.com/go/storage"
	"go.kenn.io/kit/internal/contextio"
	"go.kenn.io/kit/internal/strictio"
	"go.kenn.io/kit/objectstore"
	"google.golang.org/api/googleapi"
)

// Publish validates the whole rewindable source before uploading. Callers own
// the source and keep it stable until this call returns. A receipt proves the
// immutable key's bytes, regardless of who created them.
func (s *Store) Publish(ctx context.Context, key string, src io.ReadSeeker, size int64, digest [32]byte) (objectstore.Receipt, error) {
	full, err := s.key(key)
	if err != nil {
		return objectstore.Receipt{}, err
	}
	if src == nil || size < 0 {
		return objectstore.Receipt{}, fmt.Errorf("gcs: invalid source or size: %w", objectstore.ErrChecksumMismatch)
	}
	crc, err := validateSource(ctx, src, size, digest)
	if err != nil {
		return objectstore.Receipt{}, err
	}
	attrs, rejected, err := s.upload(ctx, full, src, size, crc)
	if err == nil {
		if attrs.Size != size || attrs.CRC32C != crc {
			return objectstore.Receipt{}, objectstore.ErrChecksumMismatch
		}
		return objectstore.Receipt{Object: s.metadata(attrs), SHA256: digest, Verification: "provider-crc32c"}, nil
	}
	if errors.Is(err, errSourceFailed) {
		return objectstore.Receipt{}, err
	}
	// The key already existed, an earlier attempt of this upload may have
	// completed before its reply was lost, or the outcome is uncertain. Even a
	// client error can follow a completed attempt, so only the stored bytes
	// can decide.
	receipt, absent, verifyErr := s.reconcile(ctx, key, size, digest)
	switch {
	case verifyErr == nil:
		return receipt, nil
	case rejected && absent:
		// The service answered with a client error and nothing is stored.
		return objectstore.Receipt{}, err
	}
	return objectstore.Receipt{}, errors.Join(verifyErr, err)
}

func validateSource(ctx context.Context, src io.ReadSeeker, size int64, digest [32]byte) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return 0, fmt.Errorf("gcs: rewind source: %w", err)
	}
	sha := sha256.New()
	crc := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	reader := &contextio.Reader{Context: ctx, Reader: src}
	n, err := io.CopyBuffer(io.MultiWriter(sha, crc), io.LimitReader(reader, size), make([]byte, 64<<10))
	if err != nil {
		return 0, err
	}
	var tail [1]byte
	more, tailErr := strictio.ReadFull(reader, tail[:])
	if err := strictio.IgnoreEOF(tailErr); err != nil {
		return 0, err
	}
	if n != size || more != 0 || string(sha.Sum(nil)) != string(digest[:]) {
		return 0, objectstore.ErrChecksumMismatch
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return 0, fmt.Errorf("gcs: rewind validated source: %w", err)
	}
	return crc.Sum32(), nil
}

// errSourceFailed marks an upload stopped before its final request because
// the source failed; the canceled upload cannot have published anything.
var errSourceFailed = errors.New("gcs publish: source failed during upload")

// upload writes the validated source with a does-not-exist precondition.
// rejected reports a client error, which proves nothing was published only
// once inspection finds no object.
func (s *Store) upload(ctx context.Context, full string, src io.Reader, size int64, crc uint32) (attrs *storage.ObjectAttrs, rejected bool, err error) {
	writeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := s.bucket.Object(full).If(storage.Conditions{DoesNotExist: true}).NewWriter(writeCtx)
	w.ContentType = "application/octet-stream"
	w.CRC32C, w.SendCRC32C = crc, true
	source := &sourceReader{reader: &contextio.Reader{Context: ctx, Reader: src}}
	n, copyErr := io.Copy(w, io.LimitReader(source, size))
	if source.err == nil && copyErr == nil {
		// LimitReader stops at size without another Read, so probe the source
		// for an error or extra bytes before Close can commit the upload.
		var tail [1]byte
		more, _ := strictio.ReadFull(source, tail[:])
		if source.err == nil && (n != size || more != 0) {
			source.err = fmt.Errorf("gcs: source changed after validation: %w", objectstore.ErrChecksumMismatch)
		}
	}
	if source.err != nil {
		// Canceling before Close keeps the final request from being sent.
		cancel()
		_ = w.Close()
		return nil, true, fmt.Errorf("%w: %w", errSourceFailed, source.err)
	}
	if err := w.Close(); err != nil {
		if apiErr, ok := errors.AsType[*googleapi.Error](err); ok && apiErr.Code == http.StatusPreconditionFailed {
			return nil, false, fmt.Errorf("gcs publish: key exists: %w", err)
		}
		return nil, isRejection(err), redactSession(err)
	}
	return w.Attrs(), false, nil
}

// isRejection reports a client error. Timeouts, throttling, and
// preconditions are not rejections.
func isRejection(err error) bool {
	apiErr, ok := errors.AsType[*googleapi.Error](err)
	if !ok {
		return false
	}
	code := apiErr.Code
	return code/100 == 4 && code != http.StatusRequestTimeout && code != http.StatusTooManyRequests && code != http.StatusPreconditionFailed
}

// redactSession keeps a resumable session URL out of errors; the URL
// authorizes uploads to the session.
func redactSession(err error) error {
	if urlErr, ok := errors.AsType[*url.Error](err); ok && strings.Contains(urlErr.URL, "upload_id=") {
		return fmt.Errorf("gcs publish: %s upload session: %w", urlErr.Op, urlErr.Err)
	}
	return fmt.Errorf("gcs publish: %w", err)
}

type sourceReader struct {
	reader io.Reader
	err    error
}

func (r *sourceReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if failure := strictio.IgnoreEOF(err); failure != nil {
		r.err = failure
	}
	return n, err
}

func unknown(err error) error { return errors.Join(objectstore.ErrOutcomeUnknown, err) }

// reconcile decides an existing key's identity from its full contents,
// pinned to the inspected generation. absent reports that the first
// inspection found neither the object nor its bucket; a later failure,
// such as the object disappearing during readback, stays uncertain.
func (s *Store) reconcile(ctx context.Context, key string, size int64, digest [32]byte) (receipt objectstore.Receipt, absent bool, err error) {
	m, err := s.Inspect(ctx, key)
	if err != nil {
		absent = errors.Is(err, objectstore.ErrNotFound) || errors.Is(err, storage.ErrBucketNotExist)
		return objectstore.Receipt{}, absent, unknown(err)
	}
	if m.Size != size {
		return objectstore.Receipt{}, false, objectstore.ErrExists
	}
	if err := s.readback(ctx, m, digest); err != nil {
		if errors.Is(err, objectstore.ErrChecksumMismatch) {
			return objectstore.Receipt{}, false, objectstore.ErrExists
		}
		return objectstore.Receipt{}, false, unknown(err)
	}
	return objectstore.Receipt{Object: m, SHA256: digest, Verification: "readback-sha256"}, false, nil
}

func (s *Store) readback(ctx context.Context, m objectstore.Metadata, digest [32]byte) error {
	sha := sha256.New()
	if m.Size > 0 {
		body, err := s.OpenRange(ctx, m.Key, m.Revision, 0, m.Size)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyBuffer(sha, body, make([]byte, 64<<10))
		if err := errors.Join(copyErr, body.Close()); err != nil {
			return err
		}
	}
	if string(sha.Sum(nil)) != string(digest[:]) {
		return objectstore.ErrChecksumMismatch
	}
	return nil
}
