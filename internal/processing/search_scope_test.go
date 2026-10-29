package processing

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func TestSearchScopeEmptyAndExclusivity(t *testing.T) {
	t.Parallel()
	f := newMediaStateFixture(t)
	request := SearchRequest{Query: "synthetic", Mode: "semantic", Profile: "speech", Fence: SourceFence{VaultUID: f.catalog.VaultID()}, Scope: &SearchScope{Kind: "all_live", Filters: &store.SearchOptions{MIMEType: "application/x-no-match"}}}
	report, err := f.service.Search(t.Context(), request)
	require.NoError(t, err)
	require.Empty(t, report.Results)
	require.Equal(t, 0, report.ObservedScopeCount)
	request.Fence.ContentVersionIDs = []string{f.version.ID}
	_, err = f.service.Search(t.Context(), request)
	require.ErrorIs(t, err, store.ErrInvalidProcessingSourceFence)
	request.Scope = nil
	request.Fence.ContentVersionIDs = nil
	_, err = f.service.Search(t.Context(), request)
	require.ErrorIs(t, err, store.ErrInvalidProcessingSourceFence)
}

func TestSearchScopeRejectsUnknownBindingWithoutEmbeddings(t *testing.T) {
	t.Parallel()
	f := newMediaStateFixture(t)
	require.Empty(t, f.service.profiles["speech"].portable.Embeddings)
	for _, mode := range []string{"lexical", "auto", "semantic", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			request := SearchRequest{Query: "synthetic", Mode: mode, Profile: "speech", BindingID: "nonexistent",
				Fence: SourceFence{VaultUID: f.catalog.VaultID()}, Scope: &SearchScope{Kind: "all_live", Filters: &store.SearchOptions{MIMEType: "application/x-no-match"}}}
			_, err := f.service.Search(t.Context(), request)
			require.ErrorIs(t, err, store.ErrNotFound)
			require.ErrorIs(t, f.service.ValidateSearch(t.Context(), request), store.ErrNotFound)
			request.BindingID = ""
			report, err := f.service.Search(t.Context(), request)
			require.NoError(t, err)
			require.Empty(t, report.Results)
			require.Zero(t, report.ObservedScopeCount)
		})
	}
	t.Run("nonempty lexical scope", func(t *testing.T) {
		request := SearchRequest{Query: "source", Mode: "lexical", Profile: "speech", BindingID: "nonexistent",
			Fence: SourceFence{VaultUID: f.catalog.VaultID()}, Scope: &SearchScope{Kind: "all_live"}}
		_, err := f.service.Search(t.Context(), request)
		require.ErrorIs(t, err, store.ErrNotFound)
	})
}

func TestLegacySearchAllowsUnusedBindingWithoutEmbeddings(t *testing.T) {
	t.Parallel()
	f := newMediaStateFixture(t)
	request := SearchRequest{Query: "source", Mode: "lexical", Profile: "speech", BindingID: "nonexistent",
		Fence: SourceFence{VaultUID: f.catalog.VaultID(), ContentVersionIDs: []string{f.version.ID}}}
	require.NoError(t, f.service.ValidateSearch(t.Context(), request))
	_, err := f.service.Search(t.Context(), request)
	require.NoError(t, err)
	request.Fence.ContentVersionIDs = nil
	require.NoError(t, f.service.ValidateSearch(t.Context(), request), "legacy preflight does not require a populated fence")
}
