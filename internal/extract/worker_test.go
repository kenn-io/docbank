package extract

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

type transientBlobReader struct {
	delegate blobReader
	failures int
	calls    int
}

func (r *transientBlobReader) OpenStreamContext(
	ctx context.Context, hash string,
) (packstore.VerifiedReadCloser, int64, error) {
	r.calls++
	if r.calls <= r.failures {
		return nil, 0, errors.New("temporary read failure")
	}
	return r.delegate.OpenStreamContext(ctx, hash)
}

type workerCatalogStub struct {
	seed    func(context.Context, string, int64) error
	pending func(context.Context, int) ([]store.ExtractionCandidate, error)
}

func (c *workerCatalogStub) SeedTextExtractionQueue(
	ctx context.Context, extractor string, version int64,
) error {
	if c.seed != nil {
		return c.seed(ctx, extractor, version)
	}
	return nil
}

func (c *workerCatalogStub) PendingTextExtractions(
	ctx context.Context, limit int,
) ([]store.ExtractionCandidate, error) {
	if c.pending != nil {
		return c.pending(ctx, limit)
	}
	return nil, nil
}

func (c *workerCatalogStub) DeferTextExtraction(
	context.Context, string, time.Time,
) error {
	return nil
}

func (c *workerCatalogStub) RecordExtraction(context.Context, store.ExtractionResult) error {
	return nil
}

type workerBlobReaderStub struct {
	calls atomic.Int32
}

func (r *workerBlobReaderStub) OpenStreamContext(
	context.Context, string,
) (packstore.VerifiedReadCloser, int64, error) {
	r.calls.Add(1)
	return nil, 0, errors.New("unexpected blob read")
}

func TestWorkerRunsStartupSeedThroughMutationGate(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var inGate atomic.Bool
	var seededInGate atomic.Bool
	var gateCalls atomic.Int32
	catalog := &workerCatalogStub{
		seed: func(context.Context, string, int64) error {
			seededInGate.Store(inGate.Load())
			cancel()
			return nil
		},
		pending: func(ctx context.Context, _ int) ([]store.ExtractionCandidate, error) {
			return nil, ctx.Err()
		},
	}
	w, err := New(catalog, &workerBlobReaderStub{}, func(ctx context.Context, fn func() error) error {
		gateCalls.Add(1)
		inGate.Store(true)
		defer inGate.Store(false)
		return fn()
	})
	require.NoError(t, err)

	require.ErrorIs(t, w.Run(ctx), context.Canceled)
	assert.True(t, seededInGate.Load())
	assert.Equal(t, int32(1), gateCalls.Load())
}

func TestWorkerStopsBeforeMutationBodyWhenAdmissionIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	admissionEntered := make(chan struct{})
	var gateCalls atomic.Int32
	catalog := &workerCatalogStub{
		pending: func(context.Context, int) ([]store.ExtractionCandidate, error) {
			return []store.ExtractionCandidate{{BlobHash: strings.Repeat("a", 64)}}, nil
		},
	}
	reader := &workerBlobReaderStub{}
	w, err := New(catalog, reader, func(ctx context.Context, fn func() error) error {
		if gateCalls.Add(1) == 1 {
			return fn()
		}
		close(admissionEntered)
		<-ctx.Done()
		return ctx.Err()
	})
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	<-admissionEntered
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	assert.Equal(t, int32(2), gateCalls.Load())
	assert.Zero(t, reader.calls.Load())
}

func TestWorkerIndexesVerifiedUTF8AndUsesMutationGate(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(s), filepath.Join(dir, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })

	content := "The lighthouse keeps a verified archive.\n"
	hash, size, err := blobs.Write(strings.NewReader(content))
	require.NoError(t, err)
	node, err := s.CreateFile(
		t.Context(), s.RootID(), "notes.md", hash, size, "text/markdown; charset=utf-8",
	)
	require.NoError(t, err)
	packed, err := blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, packed.BlobsPacked, "extraction must read through packed storage")

	gateCalls := 0
	w, err := New(s, blobs, func(_ context.Context, fn func() error) error {
		gateCalls++
		return fn()
	})
	require.NoError(t, err)
	processed, err := w.ScanOnce(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, processed)
	assert.Equal(t, 1, gateCalls)

	hits, truncated, err := s.SearchPage(t.Context(), "lighthouse", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.False(t, truncated)
	assert.Equal(t, node.ID, hits[0].Node.ID)
	assert.Equal(t, store.SearchMatchContent, hits[0].Match)

	processed, err = w.ScanOnce(t.Context())
	require.NoError(t, err)
	assert.Zero(t, processed)
	assert.Equal(t, 1, gateCalls)
}

func TestWorkerRecordsDeterministicFailuresWithoutOpeningOversizeBlob(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(s), filepath.Join(dir, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })

	// Metadata authority is enough for this branch: the size policy rejects it
	// before opening physical content.
	_, err = s.CreateFile(t.Context(), s.RootID(), "huge.txt",
		strings.Repeat("a", 64), MaxTextBytes+1, "text/plain")
	require.NoError(t, err)
	w, err := New(s, blobs, nil)
	require.NoError(t, err)
	processed, err := w.ScanOnce(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, processed)

	pending, err := s.PendingTextExtractions(t.Context(), 10)
	require.NoError(t, err)
	assert.Empty(t, pending)
}

func TestWorkerRetriesTransientOpenFailure(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(s), filepath.Join(dir, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })

	content := "A recoverable nebula record.\n"
	hash, size, err := blobs.Write(strings.NewReader(content))
	require.NoError(t, err)
	_, err = s.CreateFile(t.Context(), s.RootID(), "notes.txt", hash, size, "text/plain")
	require.NoError(t, err)
	reader := &transientBlobReader{delegate: blobs, failures: 1}
	w, err := New(s, reader, nil)
	require.NoError(t, err)
	w.interval = 5 * time.Millisecond
	w.retry = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		runErr := <-done
		if runErr != nil {
			// Cancellation can reach either the idle wait or a catalog query.
			assert.ErrorIs(t, runErr, context.Canceled)
		}
		assert.GreaterOrEqual(t, reader.calls, 2)
	})
	// This exercises real storage; busy CI runners can take more than a second.
	require.Eventually(t, func() bool {
		hits, _, searchErr := s.SearchPage(t.Context(), "nebula", 10)
		return searchErr == nil && len(hits) == 1
	}, 30*time.Second, 10*time.Millisecond)
}
