package store

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSearchUnderPathNormalization(t *testing.T) {
	t.Parallel()
	checkSearchUnderPathNormalization(t, newTestStore)
}

// This contract qualifies directory normalization, not PostgreSQL ranking.
func checkSearchUnderPathNormalization(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	dir, err := s.Mkdir(ctx, s.RootID(), "Café")
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, dir.ID, "mercury.txt", fakeHash("ab"), 4, "text/plain")
	require.NoError(t, err)
	for _, path := range []string{"/Café", "/Cafe\u0301", "/Café/"} {
		normalized, err := s.NormalizeProcessingSearchFilters(ctx, SearchOptions{UnderPath: path})
		require.NoError(t, err, path)
		require.Equal(t, dir.ID, normalized.UnderNodeID)
		require.Empty(t, normalized.UnderPath)
		again, err := s.NormalizeProcessingSearchFilters(ctx, normalized)
		require.NoError(t, err)
		require.Equal(t, normalized, again)
	}
	normalized, err := s.NormalizeProcessingSearchFilters(ctx, SearchOptions{UnderPath: "/"})
	require.NoError(t, err)
	require.Equal(t, s.RootID(), normalized.UnderNodeID)
	for _, tc := range []struct {
		path string
		id   int64
		want error
	}{
		{"/café", 0, ErrNotFound},
		{"/missing", 0, ErrNotFound},
		{"/Café/mercury.txt", 0, ErrNotDir},
		{"/Café", dir.ID, ErrInvalidSearchScope},
		{"relative", 0, ErrInvalidSearchScope},
		{"/" + strings.Repeat("é", 8192), 0, ErrInvalidSearchScope},
	} {
		_, err := s.NormalizeProcessingSearchFilters(ctx, SearchOptions{UnderPath: tc.path, UnderNodeID: tc.id})
		require.ErrorIs(t, err, tc.want)
	}
	dir, err = s.NodeByID(ctx, dir.ID)
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, dir.ID, dir.Revision)
	require.NoError(t, err)
	_, err = s.NormalizeProcessingSearchFilters(ctx, SearchOptions{UnderPath: "/Café"})
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
	fenceID, err := s.ResolveProcessingSourceFence(ctx, ProcessingSourceFenceRequest{Filters: &SearchOptions{UnderNodeID: dir.ID}})
	require.NoError(t, err)
	fencePath, err := s.ResolveProcessingSourceFence(ctx, ProcessingSourceFenceRequest{Filters: &SearchOptions{UnderPath: "/notes"}})
	require.NoError(t, err)
	require.Equal(t, fenceID, fencePath)
}

func TestSearchUnderPathByteLimit(t *testing.T) {
	// The public schema counts characters; the store enforces UTF-8 bytes.
	require.NoError(t, ValidateSearchDirectoryScope(0, "/"+strings.Repeat("a", MaxSearchUnderPathBytes-1)))
	require.ErrorIs(t, ValidateSearchDirectoryScope(0, "/"+strings.Repeat("a", MaxSearchUnderPathBytes)), ErrInvalidSearchScope)
	require.ErrorIs(t, ValidateSearchDirectoryScope(0, "/"+strings.Repeat("é", 8192)), ErrInvalidSearchScope)
}

func TestSearchUnderPathFuzzyPinsDirectoryAcrossPhases(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	scope, err := s.Mkdir(ctx, s.RootID(), "cases")
	require.NoError(t, err)
	ordinary := discoveryFile(t, s, scope.ID, "invocie.pdf")
	fuzzy := discoveryFile(t, s, scope.ID, "invoice.pdf")
	deps := s.discoveryDependencies()
	// The existing budget boundary runs after ordinary hits and before the
	// supplemental snapshot, so this mutation needs no timing or goroutines.
	deps.budget = func(ctx context.Context) (context.Context, context.CancelFunc) {
		current, err := s.NodeByID(ctx, scope.ID)
		require.NoError(t, err)
		_, _, err = s.Move(ctx, scope.ID, s.RootID(), "renamed", current.Revision)
		require.NoError(t, err)
		replacement, err := s.Mkdir(ctx, s.RootID(), "cases")
		require.NoError(t, err)
		discoveryFile(t, s, replacement.ID, "invoice.pdf")
		return context.WithCancel(ctx)
	}
	hits, _, _, err := s.searchDiscoveryPage(ctx, "invocie", 20, SearchOptions{UnderPath: "/cases"}, deps)
	require.NoError(t, err)
	require.Len(t, hits, 2)
	require.Equal(t, ordinary.ID, hits[0].Node.ID)
	require.Equal(t, fuzzy.ID, hits[1].Node.ID)
	require.Equal(t, "/renamed/invoice.pdf", hits[1].Path)
}
