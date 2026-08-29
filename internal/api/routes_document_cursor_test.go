package api

import (
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/store"
)

func TestDocumentCursorRegistryIsBoundedAndCleansExpiredEntries(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	service := newDocumentCursorTestService(&now)
	query := documentCursorTestQuery()
	request := documentCursorTestRequest()
	for range maxLiveDocumentCursors - 1 {
		_, err := service.encodeCursors(query, []documentCursorRequest{request})
		require.NoError(t, err)
	}

	_, err := service.encodeCursors(query, []documentCursorRequest{request, request})
	require.ErrorIs(t, err, store.ErrDocumentCursorCapacity)
	assert.Len(t, service.cursors.entries, maxLiveDocumentCursors-1,
		"a rejected batch must not partially consume capacity")
	_, err = service.encodeCursors(query, []documentCursorRequest{request})
	require.NoError(t, err)
	_, err = service.encodeCursors(query, []documentCursorRequest{request})
	require.ErrorIs(t, err, store.ErrDocumentCursorCapacity)

	now = now.Add(documentCursorTTL)
	_, err = service.encodeCursors(query, []documentCursorRequest{request})
	require.NoError(t, err)
	assert.Len(t, service.cursors.entries, 1)
}

func TestDocumentCursorRegistryRestartInvalidatesAuthenticatedHandle(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	service := newDocumentCursorTestService(&now)
	cursors, err := service.encodeCursors(documentCursorTestQuery(),
		[]documentCursorRequest{documentCursorTestRequest()})
	require.NoError(t, err)

	restarted := newDocumentQueryService(Deps{
		DocumentCursorKey:    []byte("0123456789abcdef0123456789abcdef"),
		DocumentCursorNow:    func() time.Time { return now },
		DocumentCursorRandom: deterministicDocumentCursorRandom(),
	})
	_, _, err = restarted.decodeCursor(cursors[0], documentCursorTestQuery())
	require.ErrorIs(t, err, store.ErrInvalidDocumentCursor)
}

func TestDocumentCursorExpiryClassificationSurvivesRegistryDeletion(t *testing.T) {
	t.Run("repeated decode", func(t *testing.T) {
		now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
		service := newDocumentCursorTestService(&now)
		cursors, err := service.encodeCursors(documentCursorTestQuery(),
			[]documentCursorRequest{documentCursorTestRequest()})
		require.NoError(t, err)
		now = now.Add(documentCursorTTL)

		_, _, err = service.decodeCursor(cursors[0], documentCursorTestQuery())
		require.ErrorIs(t, err, store.ErrDocumentCursorExpired)
		_, _, err = service.decodeCursor(cursors[0], documentCursorTestQuery())
		require.ErrorIs(t, err, store.ErrDocumentCursorExpired)
	})

	t.Run("unrelated admission cleanup", func(t *testing.T) {
		now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
		service := newDocumentCursorTestService(&now)
		cursors, err := service.encodeCursors(documentCursorTestQuery(),
			[]documentCursorRequest{documentCursorTestRequest()})
		require.NoError(t, err)
		now = now.Add(documentCursorTTL)
		_, err = service.encodeCursors(documentCursorTestQuery(),
			[]documentCursorRequest{documentCursorTestRequest()})
		require.NoError(t, err)

		_, _, err = service.decodeCursor(cursors[0], documentCursorTestQuery())
		require.ErrorIs(t, err, store.ErrDocumentCursorExpired)
	})
}

func TestDocumentCursorRejectsRegistryAndWireExpiryMismatch(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	service := newDocumentCursorTestService(&now)
	cursors, err := service.encodeCursors(documentCursorTestQuery(),
		[]documentCursorRequest{documentCursorTestRequest()})
	require.NoError(t, err)
	service.cursors.mu.Lock()
	for handle, entry := range service.cursors.entries {
		entry.expiresAt = entry.expiresAt.Add(time.Second)
		service.cursors.entries[handle] = entry
	}
	service.cursors.mu.Unlock()

	_, _, err = service.decodeCursor(cursors[0], documentCursorTestQuery())
	require.ErrorIs(t, err, store.ErrInvalidDocumentCursor)
}

func TestDocumentCursorRegistryConcurrentEncodeDecode(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	service := newDocumentCursorTestService(&now)
	query := documentCursorTestQuery()
	const workers = 32
	const perWorker = 32
	results := make(chan string, workers*perWorker)
	errs := make(chan error, workers*perWorker)
	var group sync.WaitGroup
	for range workers {
		group.Go(func() {
			for range perWorker {
				cursors, err := service.encodeCursors(query,
					[]documentCursorRequest{documentCursorTestRequest()})
				if err != nil {
					errs <- err
					continue
				}
				position, traversal, err := service.decodeCursor(cursors[0], query)
				if err != nil {
					errs <- err
					continue
				}
				if position.Path != "/boundary.txt" || traversal != store.DocumentCatalogTraversalNext {
					errs <- errors.New("decoded cursor state changed")
					continue
				}
				results <- cursors[0]
			}
		})
	}
	group.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	close(results)
	unique := make(map[string]struct{}, workers*perWorker)
	for cursor := range results {
		unique[cursor] = struct{}{}
	}
	assert.Len(t, unique, workers*perWorker)
}

func newDocumentCursorTestService(now *time.Time) *documentQueryService {
	return newDocumentQueryService(Deps{
		DocumentCursorKey:    []byte("0123456789abcdef0123456789abcdef"),
		DocumentCursorNow:    func() time.Time { return *now },
		DocumentCursorRandom: deterministicDocumentCursorRandom(),
	})
}

func deterministicDocumentCursorRandom() func([]byte) error {
	var counter atomic.Uint64
	return func(destination []byte) error {
		value := counter.Add(1)
		clear(destination)
		binary.BigEndian.PutUint64(destination[len(destination)-8:], value)
		return nil
	}
}

func documentCursorTestQuery() store.DocumentCatalogQuery {
	return store.DocumentCatalogQuery{PathPrefix: "/", Sort: store.DocumentCatalogSortPath,
		Direction: store.DocumentCatalogDirectionAscending, PageSize: 50}
}

func documentCursorTestRequest() documentCursorRequest {
	return documentCursorRequest{position: store.DocumentCatalogPosition{
		Value: "/boundary.txt", Path: "/boundary.txt", NodeID: 1,
	}, traversal: store.DocumentCatalogTraversalNext}
}
