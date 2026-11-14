package store

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// This contract qualifies directory normalization, not PostgreSQL ranking.
func checkSearchUnderPathNormalization(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	// The public schema counts characters; the store enforces UTF-8 bytes.
	require.NoError(t, ValidateSearchDirectoryScope(0, "/"+strings.Repeat("a", MaxSearchUnderPathBytes-1)))
	require.ErrorIs(t, ValidateSearchDirectoryScope(0, "/"+strings.Repeat("a", MaxSearchUnderPathBytes)), ErrInvalidSearchScope)
	require.ErrorIs(t, ValidateSearchDirectoryScope(0, "/"+strings.Repeat("é", 8192)), ErrInvalidSearchScope)
	dir, err := s.Mkdir(ctx, s.RootID(), "Café")
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, dir.ID, "mercury.txt", fakeHash("ab"), 4, "text/plain")
	require.NoError(t, err)
	for _, path := range []string{"/Café", "/Cafe\u0301", "/Café/"} {
		normalized, err := s.NormalizeSearchOptions(ctx, SearchOptions{UnderPath: path})
		require.NoError(t, err, path)
		require.Equal(t, dir.ID, normalized.UnderNodeID)
		require.Empty(t, normalized.UnderPath)
		again, err := s.NormalizeSearchOptions(ctx, normalized)
		require.NoError(t, err)
		require.Equal(t, normalized, again)
	}
	normalized, err := s.NormalizeSearchOptions(ctx, SearchOptions{UnderPath: "/"})
	require.NoError(t, err)
	require.Equal(t, s.RootID(), normalized.UnderNodeID)
	for _, tc := range []struct {
		path string
		id   int64
		want error
	}{
		{"/café", 0, ErrNotFound},
		{"/missing", 0, ErrNotFound},
		{"/missing/..", 0, ErrNotFound},
		{"/Café/..", 0, ErrInvalidSearchScope},
		{"/Café/mercury.txt", 0, ErrNotDir},
		{"/Café", dir.ID, ErrInvalidSearchScope},
		{"relative", 0, ErrInvalidSearchScope},
		{"/" + strings.Repeat("é", 8192), 0, ErrInvalidSearchScope},
	} {
		_, err := s.NormalizeSearchOptions(ctx, SearchOptions{UnderPath: tc.path, UnderNodeID: tc.id})
		require.ErrorIs(t, err, tc.want)
	}
	dir, err = s.NodeByID(ctx, dir.ID)
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, dir.ID, dir.Revision)
	require.NoError(t, err)
	_, err = s.NormalizeSearchOptions(ctx, SearchOptions{UnderPath: "/Café"})
	require.ErrorIs(t, err, ErrNotFound)
}

func TestSearchUnderPathMatchesDirectoryID(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	dir, err := s.Mkdir(ctx, s.RootID(), "notes")
	require.NoError(t, err)
	nested, err := s.Mkdir(ctx, dir.ID, "nested")
	require.NoError(t, err)
	for index, parent := range []int64{dir.ID, nested.ID, s.RootID()} {
		_, err := s.CreateFile(ctx, parent, "mercury.txt", fakeHash(string(rune('a'+index))), 4, "text/plain")
		require.NoError(t, err)
	}
	byID, moreID, err := s.SearchPageWithOptions(ctx, "mercury", 10, SearchOptions{UnderNodeID: dir.ID})
	require.NoError(t, err)
	require.Len(t, byID, 2)
	byPath, morePath, err := s.SearchPageWithOptions(ctx, "mercury", 10, SearchOptions{UnderPath: "/notes"})
	require.NoError(t, err)
	require.Equal(t, byID, byPath)
	require.Equal(t, moreID, morePath)
	normalized, err := s.NormalizeProcessingSearchFilters(ctx, SearchOptions{UnderPath: "/notes"})
	require.NoError(t, err)
	require.Equal(t, dir.ID, normalized.UnderNodeID)
	require.Empty(t, normalized.UnderPath)
	fenceID, err := s.ResolveProcessingSourceFence(ctx, ProcessingSourceFenceRequest{Filters: &SearchOptions{UnderNodeID: dir.ID}})
	require.NoError(t, err)
	fencePath, err := s.ResolveProcessingSourceFence(ctx, ProcessingSourceFenceRequest{Filters: &SearchOptions{UnderPath: "/notes"}})
	require.NoError(t, err)
	require.Equal(t, fenceID, fencePath)
}
