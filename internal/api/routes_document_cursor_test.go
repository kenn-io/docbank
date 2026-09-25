package api

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
	"testing"
	"time"
)

func newDocumentCursorTestService(now *time.Time) *documentQueryService {
	return newDocumentQueryService(Deps{DocumentCursorKey: []byte("0123456789abcdef0123456789abcdef"),
		DocumentCursorNow: func() time.Time { return *now }})
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

func TestDocumentCursorPagingHasNoProcessWideCapacity(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	service := newDocumentCursorTestService(&now)
	query, request := documentCursorTestQuery(), documentCursorTestRequest()
	for range 5000 {
		cursors, err := service.encodeCursors(query, []documentCursorRequest{request})
		require.NoError(t, err)
		position, traversal, err := service.decodeCursor(cursors[0], query)
		require.NoError(t, err)
		require.Equal(t, request.position, position)
		require.Equal(t, request.traversal, traversal)
	}
}

func TestDocumentCursorIsSelfContained(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	query, request := documentCursorTestQuery(), documentCursorTestRequest()
	first, second := newDocumentCursorTestService(&now), newDocumentCursorTestService(&now)
	cursors, err := first.encodeCursors(query, []documentCursorRequest{request})
	require.NoError(t, err)
	position, traversal, err := second.decodeCursor(cursors[0], query)
	require.NoError(t, err)
	require.Equal(t, request.position, position)
	require.Equal(t, request.traversal, traversal)
	now = now.Add(-time.Second)
	_, _, err = second.decodeCursor(cursors[0], query)
	require.ErrorIs(t, err, store.ErrInvalidDocumentCursor)
}
