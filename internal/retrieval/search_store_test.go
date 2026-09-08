package retrieval

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func TestRerankingOnlyRevalidatesCurrentCandidates(t *testing.T) {
	for _, change := range []string{"unchanged", "ancestor moved", "moved outside scope", "trashed", "content changed"} {
		t.Run(change, func(t *testing.T) {
			catalog, err := store.Open(filepath.Join(t.TempDir(), "search.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, catalog.Close()) })
			ancestor, err := catalog.Mkdir(t.Context(), catalog.RootID(), "before")
			require.NoError(t, err)
			file, err := catalog.CreateFile(t.Context(), ancestor.ID, "needle.txt", strings.Repeat("a", 64), 1, "text/plain")
			require.NoError(t, err)
			backend := &changingSearchBackend{Store: catalog, afterSearch: func() {
				switch change {
				case "ancestor moved":
					ancestor, err = catalog.NodeByID(t.Context(), ancestor.ID)
					require.NoError(t, err)
					_, _, err = catalog.Move(t.Context(), ancestor.ID, catalog.RootID(), "after", ancestor.Revision)
				case "moved outside scope":
					_, _, err = catalog.Move(t.Context(), file.ID, catalog.RootID(), file.Name, file.Revision)
				case "trashed":
					_, _, err = catalog.Trash(t.Context(), file.ID, file.Revision)
				case "content changed":
					_, _, err = catalog.ReplaceContent(t.Context(), file.ID, file.Revision, strings.Repeat("b", 64), 2, "text/plain")
				}
				require.NoError(t, err)
			}}
			provider := &stageReranker{}
			searcher, err := NewSearcher(SearcherConfig{Backend: backend, Owner: "search-test", LeaseDuration: time.Minute,
				Reranking: RerankingConfig{Enabled: true, Profile: RerankingProfile{ID: "reranking", MaxCandidates: 10},
					Provider: provider, Authorizer: &stageAuthorizer{}, Deadline: time.Second, FailurePolicy: ProviderFailureFailClosed}})
			require.NoError(t, err)
			report, err := searcher.Search(t.Context(), Query{Text: "needle", Mode: ModeLexical,
				Scope: store.SearchOptions{UnderNodeID: ancestor.ID}})
			require.NoError(t, err)
			if change == "unchanged" || change == "ancestor moved" {
				require.Len(t, report.Results, 1)
				require.Len(t, provider.candidates, 1)
				assert.Equal(t, file.ID, provider.candidates[0].Document.NodeID)
				wantPath := "/before/needle.txt"
				if change == "ancestor moved" {
					wantPath = "/after/needle.txt"
				}
				assert.Equal(t, wantPath, report.Results[0].Path)
			} else {
				assert.Empty(t, provider.candidates)
				assert.Empty(t, report.Results)
				assert.True(t, report.Truncated)
			}
		})
	}
}

type changingSearchBackend struct {
	*store.Store

	afterSearch func()
}

func (backend *changingSearchBackend) SearchExplainedLexicalCandidates(ctx context.Context, query string, limit int,
	options store.SearchOptions,
) ([]store.ExplainedLexicalCandidate, bool, error) {
	candidates, truncated, err := backend.Store.SearchExplainedLexicalCandidates(ctx, query, limit, options)
	if err == nil {
		backend.afterSearch()
	}
	return candidates, truncated, err
}

func TestExpandedSearchRefreshesPathsAfterAncestorMove(t *testing.T) {
	catalog, err := store.Open(filepath.Join(t.TempDir(), "search.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	ancestor, err := catalog.Mkdir(t.Context(), catalog.RootID(), "before")
	require.NoError(t, err)
	file, err := catalog.CreateFile(t.Context(), ancestor.ID, "needle.txt", strings.Repeat("a", 64), 1, "text/plain")
	require.NoError(t, err)
	searcher, err := NewSearcher(SearcherConfig{Backend: catalog, Owner: "search-test", LeaseDuration: time.Minute})
	require.NoError(t, err)
	query := Query{Text: "needle", Mode: ModeLexical, Limit: 10}
	report, err := searcher.Search(t.Context(), query)
	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	assert.Equal(t, "/before/needle.txt", report.Results[0].Path)

	ancestor, err = catalog.NodeByID(t.Context(), ancestor.ID)
	require.NoError(t, err)
	_, _, err = catalog.Move(t.Context(), ancestor.ID, catalog.RootID(), "after", ancestor.Revision)
	require.NoError(t, err)
	current, err := catalog.NodeByID(t.Context(), file.ID)
	require.NoError(t, err)
	require.Equal(t, file.Revision, current.Revision)

	revalidated, err := searcher.revalidateReport(t.Context(), query, report)
	require.NoError(t, err)
	require.Len(t, revalidated.Results, 1)
	assert.Equal(t, "/after/needle.txt", revalidated.Results[0].Path)
}

func TestExpansionValidatesScopeBeforeAuthorizationAndProvider(t *testing.T) {
	catalog, err := store.Open(filepath.Join(t.TempDir(), "search.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	file, err := catalog.CreateFile(t.Context(), catalog.RootID(), "needle.txt", strings.Repeat("a", 64), 1, "text/plain")
	require.NoError(t, err)
	for _, scope := range []store.SearchOptions{
		{TagID: "missing-tag"},
		{MIMEType: "text/*"},
		{MIMEType: "text/plain; charset=utf-8"},
		{UnderNodeID: -1},
		{UnderNodeID: file.ID},
		{UnderNodeID: 99999},
		{ModifiedSince: "yesterday"},
	} {
		authorizer := &stageAuthorizer{}
		provider := &stageExpander{}
		searcher, err := NewSearcher(SearcherConfig{Backend: catalog, Owner: "search-test", LeaseDuration: time.Minute,
			Expansion: ExpansionConfig{Enabled: true, Profile: ExpansionProfile{ID: "expansion", MaxVariants: 1},
				Provider: provider, Authorizer: authorizer, Deadline: time.Second, FailurePolicy: ProviderFailureFailClosed}})
		require.NoError(t, err)

		_, err = searcher.Search(t.Context(), Query{Text: "needle", Mode: ModeLexical, Scope: scope})
		require.Error(t, err)
		assert.Empty(t, authorizer.operations, "invalid scope: %+v", scope)
		assert.Zero(t, provider.calls, "invalid scope: %+v", scope)
	}

	authorizer := &stageAuthorizer{}
	provider := &stageExpander{}
	searcher, err := NewSearcher(SearcherConfig{Backend: catalog, Owner: "search-test", LeaseDuration: time.Minute,
		Expansion: ExpansionConfig{Enabled: true, Profile: ExpansionProfile{ID: "expansion", MaxVariants: 1},
			Provider: provider, Authorizer: authorizer, Deadline: time.Second, FailurePolicy: ProviderFailureFailClosed}})
	require.NoError(t, err)
	report, err := searcher.Search(t.Context(), Query{Text: "needle", Mode: ModeLexical,
		Scope: store.SearchOptions{MIMEType: "Text/Plain", UnderNodeID: catalog.RootID(),
			ModifiedSince: "2026-01-01T01:00:00+01:00"}})
	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	assert.Equal(t, 1, provider.calls)
	require.Len(t, authorizer.operations, 1)
	assert.Equal(t, store.SearchOptions{MIMEType: "text/plain", UnderNodeID: catalog.RootID(),
		ModifiedSince: "2026-01-01T00:00:00.000000000Z"}, authorizer.operations[0].Scope)
}
