package objectstore_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/objectstore"
	"go.kenn.io/kit/objectstore/local"
)

// rangeSpy records range requests and rejects zero-length ranges, as the
// cloud stores do.
type rangeSpy struct {
	objectstore.ReadStore
	t        *testing.T
	revision string
	calls    [][2]int64
}

func (s *rangeSpy) Inspect(ctx context.Context, key string) (objectstore.Metadata, error) {
	meta, err := s.ReadStore.Inspect(ctx, key)
	meta.Revision = s.revision
	return meta, err
}

func (s *rangeSpy) OpenRange(ctx context.Context, key, revision string, offset, length int64) (io.ReadCloser, error) {
	require.Positive(s.t, length, "zero-length range requested")
	if revision != s.revision {
		return nil, objectstore.ErrRevisionChanged
	}
	s.calls = append(s.calls, [2]int64{offset, length})
	return s.ReadStore.OpenRange(ctx, key, "", offset, length)
}

func publish(t *testing.T, content []byte) (*rangeSpy, objectstore.Metadata) {
	t.Helper()
	store, err := local.New(t.TempDir())
	require.NoError(t, err)
	receipt, err := store.Publish(t.Context(), "obj", bytes.NewReader(content), int64(len(content)), sha256.Sum256(content))
	require.NoError(t, err)
	meta := receipt.Object
	meta.Revision = "rev-1"
	return &rangeSpy{ReadStore: store, t: t, revision: "rev-1"}, meta
}

func TestReaderContract(t *testing.T) {
	content := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	spy, meta := publish(t, content)
	r := objectstore.NewReader(t.Context(), spy, meta)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	require.NoError(t, iotest.TestReader(r, content))
}

func TestReaderStreamsSequentialReads(t *testing.T) {
	spy, meta := publish(t, []byte("0123456789"))
	r := objectstore.NewReader(t.Context(), spy, meta)
	buf := make([]byte, 3)

	for _, want := range []string{"012", "345"} {
		n, err := r.Read(buf)
		require.NoError(t, err)
		assert.Equal(t, want, string(buf[:n]))
	}
	_, err := r.Seek(8, io.SeekStart)
	require.NoError(t, err)
	rest, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, "89", string(rest))

	assert.Equal(t, [][2]int64{{0, 10}, {8, 2}}, spy.calls)

	// Data remains at offset 0, so only the closed state can stop this read.
	_, err = r.Seek(0, io.SeekStart)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	n, err := r.Read(buf)
	require.Error(t, err)
	require.NotErrorIs(t, err, io.EOF)
	assert.Zero(t, n)
	assert.Len(t, spy.calls, 2)
}

func TestReaderAtShortTail(t *testing.T) {
	spy, meta := publish(t, []byte("0123456789"))
	r := objectstore.NewReader(t.Context(), spy, meta)
	buf := make([]byte, 4)

	n, err := r.ReadAt(buf, 8)
	require.ErrorIs(t, err, io.EOF)
	assert.Equal(t, "89", string(buf[:n]))
	assert.Equal(t, [][2]int64{{8, 2}}, spy.calls)
}

func TestReaderPinsRevision(t *testing.T) {
	spy, meta := publish(t, []byte("0123456789"))
	r := objectstore.NewReader(t.Context(), spy, meta)
	spy.revision = "rev-2"

	_, err := r.ReadAt(make([]byte, 2), 0)
	require.ErrorIs(t, err, objectstore.ErrRevisionChanged)
	_, err = r.Read(make([]byte, 2))
	assert.ErrorIs(t, err, objectstore.ErrRevisionChanged)
}

func TestEmptyObjectNeedsNoRange(t *testing.T) {
	spy, meta := publish(t, nil)

	body, got, err := objectstore.Open(t.Context(), spy, "obj")
	require.NoError(t, err)
	data, err := io.ReadAll(body)
	require.NoError(t, err)
	require.NoError(t, body.Close())
	assert.Empty(t, data)
	assert.Zero(t, got.Size)

	r := objectstore.NewReader(t.Context(), spy, meta)
	n, err := r.Read(make([]byte, 4))
	assert.Zero(t, n)
	require.ErrorIs(t, err, io.EOF)
	assert.Empty(t, spy.calls)
}

func TestOpenReadsWholeObjectAtInspectedRevision(t *testing.T) {
	spy, _ := publish(t, []byte("hello world"))

	body, meta, err := objectstore.Open(t.Context(), spy, "obj")
	require.NoError(t, err)
	data, err := io.ReadAll(body)
	require.NoError(t, err)
	require.NoError(t, body.Close())
	assert.Equal(t, "hello world", string(data))
	assert.Equal(t, "rev-1", meta.Revision)
	assert.Equal(t, [][2]int64{{0, 11}}, spy.calls)

	_, _, err = objectstore.Open(t.Context(), spy, "missing")
	assert.ErrorIs(t, err, objectstore.ErrNotFound)
}

// finalBytesWithError serves its data in one read together with err, as a
// client does when an integrity check fails on the last chunk.
type finalBytesWithError struct {
	data []byte
	err  error
}

func (s finalBytesWithError) OpenRange(_ context.Context, _, _ string, offset, length int64) (io.ReadCloser, error) {
	return io.NopCloser(&finalRead{data: s.data[offset : offset+length], err: s.err}), nil
}

type finalRead struct {
	data []byte
	err  error
}

func (r *finalRead) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.err
	}
	return n, nil
}

func TestReadsKeepErrorWithFinalBytes(t *testing.T) {
	errIntegrity := errors.New("bad CRC on read")
	for name, final := range map[string]error{
		"alone":         errIntegrity,
		"joined to EOF": errors.Join(io.EOF, errIntegrity),
	} {
		t.Run(name, func(t *testing.T) {
			store := finalBytesWithError{data: []byte("0123456789"), err: final}
			r := objectstore.NewReader(t.Context(), store, objectstore.Metadata{Key: "obj", Size: 10})

			n, err := r.ReadAt(make([]byte, 4), 2)
			require.ErrorIs(t, err, errIntegrity)
			assert.Equal(t, 4, n)
			n, err = r.ReadAt(make([]byte, 10), 0)
			require.ErrorIs(t, err, errIntegrity)
			assert.Equal(t, 10, n)

			n, err = r.Read(make([]byte, 10))
			require.ErrorIs(t, err, errIntegrity)
			assert.Equal(t, 10, n)
		})
	}
}

// shortRange serves fewer bytes than requested, ending with err.
type shortRange struct {
	data string
	err  error
}

func (s shortRange) OpenRange(context.Context, string, string, int64, int64) (io.ReadCloser, error) {
	return io.NopCloser(&finalRead{data: []byte(s.data), err: s.err}), nil
}

func TestReadAtShortRangeKeepsCause(t *testing.T) {
	errCause := errors.New("connection reset")
	r := objectstore.NewReader(t.Context(), shortRange{data: "abc", err: errors.Join(io.EOF, errCause)}, objectstore.Metadata{Key: "obj", Size: 5})
	n, err := r.ReadAt(make([]byte, 5), 0)
	assert.Equal(t, 3, n)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.ErrorIs(t, err, errCause)
}

// errorAfterFinalBytes serves its data without an error, then fails the next
// read, as a client does when it checks integrity only after the last byte.
type errorAfterFinalBytes struct {
	data []byte
	err  error
}

func (s errorAfterFinalBytes) OpenRange(_ context.Context, _, _ string, offset, length int64) (io.ReadCloser, error) {
	return io.NopCloser(&lateError{data: s.data[offset : offset+length], err: s.err}), nil
}

type lateError struct {
	data []byte
	err  error
}

func (r *lateError) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestReadsObserveErrorAfterFinalBytes(t *testing.T) {
	errIntegrity := errors.New("bad CRC after body")
	store := errorAfterFinalBytes{data: []byte("0123456789"), err: errIntegrity}

	r := objectstore.NewReader(t.Context(), store, objectstore.Metadata{Key: "obj", Size: 10})
	data, err := io.ReadAll(r)
	require.ErrorIs(t, err, errIntegrity)
	assert.Equal(t, "0123456789", string(data))
	require.NoError(t, r.Close())

	n, err := objectstore.NewReader(t.Context(), store, objectstore.Metadata{Key: "obj", Size: 10}).ReadAt(make([]byte, 4), 6)
	require.ErrorIs(t, err, errIntegrity)
	assert.Equal(t, 4, n)
}

func TestReadAtRejectsRangeLongerThanRequested(t *testing.T) {
	r := objectstore.NewReader(t.Context(), shortRange{data: "abcdef", err: io.EOF}, objectstore.Metadata{Key: "obj", Size: 5})
	_, err := r.ReadAt(make([]byte, 5), 0)
	require.Error(t, err)
	require.NotErrorIs(t, err, io.EOF)
}
