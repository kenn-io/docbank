package objectstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	"go.kenn.io/kit/internal/strictio"
)

// RangeOpener opens exactly length bytes at offset, guarded by a revision
// when one is supplied. Every concrete store implements it. Length is always
// positive when called by this package.
type RangeOpener interface {
	OpenRange(ctx context.Context, key, revision string, offset, length int64) (io.ReadCloser, error)
}

// ReadStore inspects and reads objects. Every concrete store implements it.
type ReadStore interface {
	RangeOpener
	Inspect(ctx context.Context, key string) (Metadata, error)
}

// Open inspects key and streams its entire contents, pinned to the observed
// revision. Stores without revisions return an empty Metadata.Revision and
// read whatever is at the key.
func Open(ctx context.Context, store ReadStore, key string) (io.ReadCloser, Metadata, error) {
	meta, err := store.Inspect(ctx, key)
	if err != nil {
		return nil, Metadata{}, err
	}
	if meta.Size == 0 {
		return io.NopCloser(bytes.NewReader(nil)), meta, nil
	}
	body, err := store.OpenRange(ctx, key, meta.Revision, 0, meta.Size)
	if err != nil {
		return nil, Metadata{}, err
	}
	return body, meta, nil
}

// Reader reads one object as an io.ReaderAt and io.ReadSeeker. Every range
// request is pinned to the object's Metadata.Revision, so a replaced object
// fails with the store's revision error instead of mixing contents.
//
// Read streams sequentially through a single range request that is reopened
// only after Seek moves the position. ReadAt issues one range request per
// call, never touches the Read position, and is safe for concurrent use.
// Read, Seek, and Close are not safe for concurrent use with each other.
type Reader struct {
	// ctx bounds every range request; io interfaces carry no context.
	ctx    context.Context
	store  RangeOpener
	meta   Metadata
	closed atomic.Bool

	pos     int64
	body    io.ReadCloser
	bodyPos int64
}

// NewReader returns a Reader over the object described by meta, normally
// from Inspect or a Receipt. ctx bounds every range request the Reader makes
// until Close. It performs no request until the first read.
func NewReader(ctx context.Context, store RangeOpener, meta Metadata) *Reader {
	return &Reader{ctx: ctx, store: store, meta: meta}
}

// Size returns the object's size in bytes.
func (r *Reader) Size() int64 { return r.meta.Size }

// Metadata returns the metadata the Reader was created with.
func (r *Reader) Metadata() Metadata { return r.meta }

var errClosed = errors.New("objectstore: reader closed")

// ReadAt reads len(p) bytes at off with one range request. It returns io.EOF
// when fewer bytes remain than requested.
func (r *Reader) ReadAt(p []byte, off int64) (int, error) {
	if r.closed.Load() {
		return 0, errClosed
	}
	if off < 0 {
		return 0, errors.New("objectstore: negative offset")
	}
	if off >= r.meta.Size {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	length := min(int64(len(p)), r.meta.Size-off)
	body, err := r.store.OpenRange(r.ctx, r.meta.Key, r.meta.Revision, off, length)
	if err != nil {
		return 0, err
	}
	n, err := strictio.ReadFull(body, p[:length])
	err = strictio.RangeEnd(err, length-int64(n))
	if err == nil {
		err = finishRange(body)
	}
	if err = errors.Join(err, body.Close()); err != nil {
		return n, err
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// Read streams from the current position.
func (r *Reader) Read(p []byte) (int, error) {
	if r.closed.Load() {
		return 0, errClosed
	}
	if r.pos >= r.meta.Size {
		return 0, r.finish()
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.body != nil && r.bodyPos != r.pos {
		// The abandoned stream's bytes are never returned, so its close
		// result cannot affect what the caller reads.
		_ = r.body.Close()
		r.body = nil
	}
	if r.body == nil {
		body, err := r.store.OpenRange(r.ctx, r.meta.Key, r.meta.Revision, r.pos, r.meta.Size-r.pos)
		if err != nil {
			return 0, err
		}
		r.body, r.bodyPos = body, r.pos
	}
	n, err := r.body.Read(p)
	r.pos += int64(n)
	r.bodyPos = r.pos
	if err == nil {
		return n, nil
	}
	closeErr := r.body.Close()
	r.body = nil
	// A wrapped or joined EOF carries another failure; only a plain EOF ends
	// the stream cleanly.
	if err != io.EOF {
		return n, errors.Join(err, closeErr)
	}
	if r.pos < r.meta.Size {
		return n, errors.Join(io.ErrUnexpectedEOF, closeErr)
	}
	if closeErr != nil {
		return n, closeErr
	}
	if n > 0 {
		return n, nil
	}
	return 0, io.EOF
}

// finish ends Read at the object's end. A stream that delivered its final
// bytes without an error must still report a failure that arrives on the next
// read, such as an integrity check, before Read can return io.EOF.
func (r *Reader) finish() error {
	if r.body == nil {
		return io.EOF
	}
	body := r.body
	r.body = nil
	if r.bodyPos != r.pos {
		// Seek moved past the end, so this stream's bytes are never returned.
		_ = body.Close()
		return io.EOF
	}
	if err := errors.Join(finishRange(body), body.Close()); err != nil {
		return err
	}
	return io.EOF
}

// finishRange reads once past a range's expected bytes. Only a plain io.EOF
// confirms that the body ended cleanly.
func finishRange(body io.Reader) error {
	var extra [1]byte
	n, err := strictio.ReadFull(body, extra[:])
	if err := strictio.IgnoreEOF(err); err != nil {
		return err
	}
	if n != 0 {
		return errors.New("objectstore: range body exceeds requested length")
	}
	return nil
}

// Seek sets the position for the next Read. Seeking past the end is allowed;
// Read then returns io.EOF.
func (r *Reader) Seek(offset int64, whence int) (int64, error) {
	if r.closed.Load() {
		return 0, errClosed
	}
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.pos + offset
	case io.SeekEnd:
		abs = r.meta.Size + offset
	default:
		return 0, fmt.Errorf("objectstore: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, errors.New("objectstore: negative position")
	}
	r.pos = abs
	return abs, nil
}

// Close releases any open stream. Later reads fail; repeated Close succeeds.
func (r *Reader) Close() error {
	if r.closed.Swap(true) || r.body == nil {
		return nil
	}
	err := r.body.Close()
	r.body = nil
	return err
}
