package processing

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func TestCoverageScopeMatchesExplicitAndEchoesNormalizedFilters(t *testing.T) {
	t.Parallel()
	f := newMediaStateFixture(t)
	fence := SourceFence{VaultUID: f.catalog.VaultID(), ContentVersionIDs: []string{f.version.ID}}
	exact, err := f.service.Coverage(t.Context(), "speech", fence)
	require.NoError(t, err)
	require.Nil(t, exact.Scope)
	filters := &store.SearchOptions{MIMEType: "AUDIO/WAV"}
	fence.ContentVersionIDs = nil
	scoped, err := f.service.CoverageWithScope(t.Context(), "speech", fence, &SearchScope{Kind: "all_live", Filters: filters})
	require.NoError(t, err)
	require.Equal(t, exact.Renditions, scoped.Renditions)
	require.Equal(t, exact.Embeddings, scoped.Embeddings)
	require.Equal(t, 1, scoped.ObservedScopeCount)
	require.Equal(t, "all_live", scoped.Scope.Kind)
	require.Equal(t, "audio/wav", scoped.Scope.Filters.MIMEType)
	empty, err := f.service.CoverageWithScope(t.Context(), "speech", fence, &SearchScope{Kind: "all_live", Filters: &store.SearchOptions{MIMEType: "APPLICATION/X-NO-MATCH"}})
	require.NoError(t, err)
	require.Equal(t, "complete", empty.State)
	require.Zero(t, empty.Renditions.Total)
	require.Equal(t, "application/x-no-match", empty.Scope.Filters.MIMEType)
}

func TestCoverageScopeRejectsInvalidAuthority(t *testing.T) {
	t.Parallel()
	f := newMediaStateFixture(t)
	fence := SourceFence{VaultUID: f.catalog.VaultID()}
	for _, scope := range []*SearchScope{nil, {Kind: "unknown"}, {Kind: "all_live", Filters: &store.SearchOptions{ContentVersionIDs: []string{f.version.ID}}}, {Kind: "all_live", Filters: &store.SearchOptions{MIMEType: "malformed mime"}}} {
		_, err := f.service.CoverageWithScope(t.Context(), "speech", fence, scope)
		require.ErrorIs(t, err, store.ErrInvalidProcessingSourceFence)
	}
	fence.ContentVersionIDs = []string{f.version.ID}
	_, err := f.service.CoverageWithScope(t.Context(), "speech", fence, &SearchScope{Kind: "all_live"})
	require.ErrorIs(t, err, store.ErrInvalidProcessingSourceFence)
	fence = SourceFence{VaultUID: "foreign"}
	_, err = f.service.CoverageWithScope(t.Context(), "speech", fence, &SearchScope{Kind: "all_live"})
	require.ErrorIs(t, err, ErrForeignVault)
}
