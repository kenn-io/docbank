package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/pack"
)

// mapSource serves content from memory, keyed by ref hash. Missing keys
// return fs-agnostic errNotInSource.
type mapSource struct {
	blobs map[string][]byte
	opens atomic.Int64
}

var errNotInSource = errors.New("blob not in source")

func (s *mapSource) Open(_ context.Context, ref ContentRef) (io.ReadCloser, error) {
	s.opens.Add(1)
	b, ok := s.blobs[ref.Hash]
	if !ok {
		return nil, fmt.Errorf("%w: %s", errNotInSource, ref.Hash)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func sourceRef(content []byte) (ContentRef, string) {
	sum := sha256.Sum256(content)
	h := hex.EncodeToString(sum[:])
	return ContentRef{Hash: h, Size: int64(len(content))}, h
}

// newTestAppenderForSource builds a *PackAppender against a fresh temp repo
// and returns it alongside the repo and the live known-blob map the appender
// mutates as it appends blobs. This mirrors the initTestRepo(t) +
// NewPackAppender(...) construction attachments_test.go repeats inline at
// each call site; the known map is returned (rather than rebuilt from
// Finish's entries, as attachments_test.go does) because PackAppender
// mutates it in place, so it is already complete once capture finishes.
func newTestAppenderForSource(t *testing.T) (*PackAppender, *Repo, map[pack.BlobID]IndexEntry) {
	t.Helper()
	r := initTestRepo(t)
	known := map[pack.BlobID]IndexEntry{}
	appender := NewPackAppender(r, known, pack.DefaultZstdLevel, nil, testPackExt)
	return appender, r, known
}

// assertRepoHoldsBlob asserts that repo holds want under hash, resolved
// through known — the same r.ReadBlob(known, id, nil, testPackExt) call
// attachments_test.go's TestCaptureAttachments performs after Finish.
func assertRepoHoldsBlob(t *testing.T, r *Repo, known map[pack.BlobID]IndexEntry, hash string, want []byte) {
	t.Helper()
	id, err := pack.ParseBlobID(hash)
	require.NoError(t, err)
	got, err := r.ReadBlob(known, id, nil, testPackExt)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestCaptureAttachmentsFromSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	a := []byte("alpha content")
	b := []byte("bravo content")
	refA, hashA := sourceRef(a)
	refB, hashB := sourceRef(b)
	src := &mapSource{blobs: map[string][]byte{hashA: a, hashB: b}}

	appender, repo, known := newTestAppenderForSource(t)
	out, err := CaptureAttachments(t.Context(), "", []ContentRef{refA, refB},
		map[string]bool{}, appender, CaptureOptions{Source: src})
	require.NoError(err)
	_, _, err = appender.Finish()
	require.NoError(err)

	assert.Equal(int64(2), out.Blobs)
	assert.Equal(int64(len(a)+len(b)), out.BlobBytes)
	assert.Len(out.NewList, 2)
	assert.Equal(hashA, out.NewList[0].Hash)
	assert.Equal(int64(2), src.opens.Load())
	assertRepoHoldsBlob(t, repo, known, hashA, a)
	assertRepoHoldsBlob(t, repo, known, hashB, b)
}

func TestCaptureFromSourceHashMismatch(t *testing.T) {
	require := require.New(t)

	refA, hashA := sourceRef([]byte("expected content"))
	src := &mapSource{blobs: map[string][]byte{hashA: []byte("tampered content!")}}

	appender, _, _ := newTestAppenderForSource(t)
	defer appender.Abort()
	_, err := CaptureAttachments(t.Context(), "", []ContentRef{refA},
		map[string]bool{}, appender, CaptureOptions{Source: src})
	require.Error(err)
	require.Contains(err.Error(), "does not match its hash")
}

func TestCaptureFromSourceRejectsNoncanonicalHash(t *testing.T) {
	require := require.New(t)
	content := []byte("canonical source hash")
	ref, hash := sourceRef(content)
	ref.Hash = strings.ToUpper(hash)
	src := &mapSource{blobs: map[string][]byte{ref.Hash: content}}

	appender, _, _ := newTestAppenderForSource(t)
	defer appender.Abort()
	_, err := CaptureAttachments(t.Context(), "", []ContentRef{ref},
		map[string]bool{}, appender, CaptureOptions{Source: src})
	require.ErrorContains(err, "not canonical lowercase hex")
}

func TestCaptureFromSourceMissingBlob(t *testing.T) {
	require := require.New(t)

	refA, _ := sourceRef([]byte("never stored"))
	src := &mapSource{blobs: map[string][]byte{}}

	appender, _, _ := newTestAppenderForSource(t)
	defer appender.Abort()
	_, err := CaptureAttachments(t.Context(), "", []ContentRef{refA},
		map[string]bool{}, appender, CaptureOptions{Source: src})
	require.ErrorIs(err, errNotInSource)
}

func TestCaptureFromSourceOversizedBlob(t *testing.T) {
	require := require.New(t)

	old := maxCaptureRawLen
	maxCaptureRawLen = 8
	t.Cleanup(func() { maxCaptureRawLen = old })

	content := []byte("longer than eight bytes")
	refA, hashA := sourceRef(content)
	src := &mapSource{blobs: map[string][]byte{hashA: content}}

	for _, size := range []int64{refA.Size, -1} {
		refA.Size = size
		appender, _, _ := newTestAppenderForSource(t)
		defer appender.Abort()
		_, err := CaptureAttachments(t.Context(), "", []ContentRef{refA},
			map[string]bool{}, appender, CaptureOptions{Source: src})
		require.Error(err)
		require.Contains(err.Error(), "maximum blob size")
	}
}

func TestCaptureFromSourceIgnoresStoragePath(t *testing.T) {
	// A source is keyed on the ref, not the filesystem: a noncanonical
	// StoragePath (importer namespace) must not affect source capture.
	require := require.New(t)

	content := []byte("namespaced blob")
	refA, hashA := sourceRef(content)
	refA.StoragePath = "synctech-sms/" + hashA[:2] + "/" + hashA
	src := &mapSource{blobs: map[string][]byte{hashA: content}}

	appender, _, _ := newTestAppenderForSource(t)
	defer appender.Abort()
	out, err := CaptureAttachments(t.Context(), "", []ContentRef{refA},
		map[string]bool{}, appender, CaptureOptions{Source: src})
	require.NoError(err)
	require.Equal(int64(1), out.Blobs)
}

func TestCaptureFromSourceParallel(t *testing.T) {
	require := require.New(t)

	blobs := map[string][]byte{}
	var refs []ContentRef
	for i := range 40 {
		content := fmt.Appendf(nil, "blob %03d content", i)
		ref, h := sourceRef(content)
		blobs[h] = content
		refs = append(refs, ref)
	}
	src := &mapSource{blobs: blobs}

	appender, _, _ := newTestAppenderForSource(t)
	defer appender.Abort()
	out, err := CaptureAttachments(t.Context(), "", refs,
		map[string]bool{}, appender, CaptureOptions{Source: src, Jobs: 8})
	require.NoError(err)
	require.Equal(int64(40), out.Blobs)
	// Ordered collector: list order matches ref order regardless of Jobs.
	for i, ref := range refs {
		require.Equal(ref.Hash, out.NewList[i].Hash)
	}
}

type captureSourceFunc func(context.Context, ContentRef) (io.ReadCloser, error)

func (f captureSourceFunc) Open(ctx context.Context, ref ContentRef) (io.ReadCloser, error) {
	return f(ctx, ref)
}

func TestCaptureUnknownSourceUsesWorkerPool(t *testing.T) {
	const jobs = 8
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	started := make(chan struct{}, jobs)
	release := make(chan struct{})
	src := &mapSource{blobs: map[string][]byte{}}
	var refs []ContentRef
	for i := range jobs {
		body := fmt.Appendf(nil, "thumbnail %d", i)
		ref, hash := sourceRef(body)
		ref.Size = -1
		refs = append(refs, ref)
		src.blobs[hash] = body
	}
	blocked := captureSourceFunc(func(ctx context.Context, ref ContentRef) (io.ReadCloser, error) {
		started <- struct{}{}
		select {
		case <-release:
			return src.Open(ctx, ref)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	appender, repo, known := newTestAppenderForSource(t)
	defer appender.Abort()
	finished := make(chan error, 1)
	go func() {
		_, err := CaptureAttachments(ctx, "", refs, nil, appender, CaptureOptions{Jobs: jobs, Source: blocked})
		finished <- err
	}()
	opened := 0
	for opened < jobs && ctx.Err() == nil {
		select {
		case <-started:
			opened++
		case <-ctx.Done():
		}
	}
	close(release)
	err := <-finished
	require.Equal(t, jobs, opened, "unknown sizes must use the requested capture workers")
	require.NoError(t, err)
	_, _, err = appender.Finish()
	require.NoError(t, err)
	for _, ref := range refs {
		assert.Equal(t, int64(len(src.blobs[ref.Hash])), ref.Size)
		assertRepoHoldsBlob(t, repo, known, ref.Hash, src.blobs[ref.Hash])
	}
}
