package extract

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
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
	w, err := New(s, blobs, func(fn func() error) error {
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
	require.Eventually(t, func() bool {
		hits, _, searchErr := s.SearchPage(t.Context(), "nebula", 10)
		return searchErr == nil && len(hits) == 1
	}, time.Second, 5*time.Millisecond)
	cancel()
	require.NoError(t, <-done)
	assert.GreaterOrEqual(t, reader.calls, 2)
}
