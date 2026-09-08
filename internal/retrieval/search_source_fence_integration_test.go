package retrieval

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorindex"
)

func TestSearcherSourceFenceOwnsCallerAndExpansionAuthorizationScopes(t *testing.T) {
	for _, test := range []struct {
		name    string
		query   string
		variant string
		mutate  func([]string, ProviderOperation, string)
	}{
		{name: "caller slice during base query", query: "match", variant: "expanded",
			mutate: func(ids []string, _ ProviderOperation, replacement string) { ids[0] = replacement }},
		{name: "operation scope during expansion variant", query: "unmatched", variant: "match",
			mutate: func(_ []string, operation ProviderOperation, replacement string) {
				operation.Scope.ContentVersionIDs[0] = replacement
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRetrievalStoreFixture(t)
			original := fixture.target.CurrentVersionID
			excluded := fixture.safe.CurrentVersionID
			ids := []string{original}
			authorizer := expansionAuthorizerFunc(func(_ context.Context, operation ProviderOperation) error {
				require.Equal(t, []string{original}, operation.Scope.ContentVersionIDs)
				test.mutate(ids, operation, excluded)
				return nil
			})
			searcher := fixture.providerStageSearcher(t, fixture.provider(nil), ExpansionConfig{Enabled: true,
				Profile:  ExpansionProfile{ID: "expansion", MaxVariants: 1},
				Provider: &stageExpander{variants: []string{test.variant}}, Authorizer: authorizer,
				Deadline: time.Second, FailurePolicy: ProviderFailureFailClosed}, RerankingConfig{})
			query := fixture.query(ModeLexical, 2, store.SearchOptions{ContentVersionIDs: ids})
			query.Text = test.query

			report, err := searcher.Search(t.Context(), query)

			require.NoError(t, err)
			require.Len(t, report.Results, 1)
			assert.Equal(t, original, report.Results[0].Document.ContentVersionID)
			assert.Equal(t, fixture.target.ID, report.Results[0].Document.NodeID)
		})
	}
}

func TestSearcherSourceFencePreservesCallerOrderForAuthorization(t *testing.T) {
	fixture := newRetrievalStoreFixture(t)
	ids := []string{fixture.safe.CurrentVersionID, fixture.target.CurrentVersionID}
	authorizer := expansionAuthorizerFunc(func(_ context.Context, operation ProviderOperation) error {
		require.Equal(t, ids, operation.Scope.ContentVersionIDs)
		return nil
	})
	searcher := fixture.providerStageSearcher(t, fixture.provider(nil), ExpansionConfig{Enabled: true,
		Profile:  ExpansionProfile{ID: "expansion", MaxVariants: 1},
		Provider: &stageExpander{variants: []string{"expanded"}}, Authorizer: authorizer,
		Deadline: time.Second, FailurePolicy: ProviderFailureFailClosed}, RerankingConfig{})

	report, err := searcher.Search(t.Context(), fixture.query(ModeLexical, 2,
		store.SearchOptions{ContentVersionIDs: ids}))

	require.NoError(t, err)
	assert.Len(t, report.Results, 2)
}

func TestSearcherSourceFenceOwnsRerankingAuthorizationScope(t *testing.T) {
	fixture := newRetrievalStoreFixture(t)
	original := fixture.target.CurrentVersionID
	excluded := fixture.safe.CurrentVersionID
	reranker := &stageReranker{}
	authorizer := rerankingAuthorizerFunc(func(_ context.Context, operation ProviderOperation) error {
		require.Equal(t, []string{original}, operation.Scope.ContentVersionIDs)
		operation.Scope.ContentVersionIDs[0] = excluded
		return nil
	})
	searcher := fixture.providerStageSearcher(t, fixture.provider(nil), ExpansionConfig{},
		rankingConfig(reranker, authorizer, ProviderFailureFailClosed))

	report, err := searcher.Search(t.Context(), fixture.query(ModeLexical, 2,
		store.SearchOptions{ContentVersionIDs: []string{original}}))

	require.NoError(t, err)
	require.Len(t, reranker.candidates, 1)
	assert.Equal(t, original, reranker.candidates[0].Document.ContentVersionID)
	require.Len(t, report.Results, 1)
	assert.Equal(t, original, report.Results[0].Document.ContentVersionID)
}

func TestSearcherSourceFenceOwnsEveryBackendCallbackScope(t *testing.T) {
	for _, test := range []struct {
		name       string
		mode       Mode
		mutateAt   sourceFenceCallback
		reranking  bool
		wantStages []sourceFenceCallback
	}{
		{name: "lexical candidates", mode: ModeLexical, mutateAt: sourceFenceLexical,
			wantStages: []sourceFenceCallback{sourceFenceLexical, sourceFenceRevalidate}},
		{name: "semantic authority", mode: ModeHybrid, mutateAt: sourceFenceAcquire,
			wantStages: []sourceFenceCallback{sourceFenceLexical, sourceFenceAcquire,
				sourceFenceResolve, sourceFenceRevalidate}},
		{name: "semantic resolution", mode: ModeHybrid, mutateAt: sourceFenceResolve,
			wantStages: []sourceFenceCallback{sourceFenceLexical, sourceFenceAcquire,
				sourceFenceResolve, sourceFenceRevalidate}},
		{name: "candidate revalidation", mode: ModeLexical, mutateAt: sourceFenceRevalidate, reranking: true,
			wantStages: []sourceFenceCallback{sourceFenceLexical, sourceFenceRevalidate,
				sourceFenceRevalidate, sourceFenceRevalidate}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRetrievalStoreFixture(t)
			original := fixture.target.CurrentVersionID
			backend := &sourceFenceRecordingBackend{Store: fixture.store,
				replacement: fixture.safe.CurrentVersionID, mutateAt: test.mutateAt}
			config := SearcherConfig{Backend: backend, Encoders: retrievalMutatingResolver{provider: fixture.provider(nil)},
				Owner: "source-fence-integration", LeaseDuration: time.Hour,
				Clock: func() time.Time { return time.Date(2026, 9, 7, 13, 0, 0, 0, time.UTC) }}
			if test.reranking {
				config.Reranking = rankingConfig(&stageReranker{},
					rerankingAuthorizerFunc(func(_ context.Context, operation ProviderOperation) error {
						require.Equal(t, []string{original}, operation.Scope.ContentVersionIDs)
						return nil
					}), ProviderFailureFailClosed)
			}
			searcher, err := NewSearcher(config)
			require.NoError(t, err)

			report, err := searcher.Search(t.Context(), fixture.query(test.mode, 2,
				store.SearchOptions{ContentVersionIDs: []string{original}}))

			require.NoError(t, err)
			require.Len(t, report.Results, 1)
			assert.Equal(t, fixture.target.ID, report.Results[0].Document.NodeID)
			assert.Equal(t, original, report.Results[0].Document.ContentVersionID)
			assert.Equal(t, test.wantStages, backend.stages)
			for _, observed := range backend.scopes {
				assert.Equal(t, []string{original}, observed)
			}
			if test.mode == ModeHybrid {
				assert.Equal(t, Coverage{BindingRequired: true, ScopedDocuments: 1,
					CompleteDocuments: 1, State: CoverageComplete}, report.Coverage)
			}
		})
	}
}

func TestSearcherSourceFenceNilAndEmptyRemainUnrestricted(t *testing.T) {
	for _, scope := range []store.SearchOptions{{}, {ContentVersionIDs: []string{}}} {
		fixture := newRetrievalStoreFixture(t)
		searcher := fixture.searcher(t, fixture.provider(nil))

		report, err := searcher.Search(t.Context(), fixture.query(ModeLexical, 2, scope))

		require.NoError(t, err)
		require.Len(t, report.Results, 2)
		assert.Equal(t, fixture.target.ID, report.Results[0].Document.NodeID)
		assert.Equal(t, fixture.safe.ID, report.Results[1].Document.NodeID)
	}
}

type sourceFenceCallback string

const (
	sourceFenceLexical    sourceFenceCallback = "lexical"
	sourceFenceAcquire    sourceFenceCallback = "semantic_acquire"
	sourceFenceResolve    sourceFenceCallback = "semantic_resolve"
	sourceFenceRevalidate sourceFenceCallback = "revalidate"
)

type sourceFenceRecordingBackend struct {
	*store.Store

	replacement string
	mutateAt    sourceFenceCallback
	stages      []sourceFenceCallback
	scopes      [][]string
}

func (backend *sourceFenceRecordingBackend) record(scope store.SearchOptions, stage sourceFenceCallback) {
	backend.stages = append(backend.stages, stage)
	backend.scopes = append(backend.scopes, slices.Clone(scope.ContentVersionIDs))
}

func (backend *sourceFenceRecordingBackend) mutate(scope store.SearchOptions, stage sourceFenceCallback) {
	if stage == backend.mutateAt && len(scope.ContentVersionIDs) != 0 {
		scope.ContentVersionIDs[0] = backend.replacement
	}
}

func (backend *sourceFenceRecordingBackend) SearchExplainedLexicalCandidates(ctx context.Context, query string,
	limit int, scope store.SearchOptions,
) ([]store.ExplainedLexicalCandidate, bool, error) {
	backend.record(scope, sourceFenceLexical)
	candidates, truncated, err := backend.Store.SearchExplainedLexicalCandidates(ctx, query, limit, scope)
	backend.mutate(scope, sourceFenceLexical)
	return candidates, truncated, err
}

func (backend *sourceFenceRecordingBackend) AcquireSemanticSearchAuthority(ctx context.Context,
	profile, binding, owner string, at time.Time, duration time.Duration, scope store.SearchOptions,
) (store.SemanticSearchAuthority, error) {
	backend.record(scope, sourceFenceAcquire)
	authority, err := backend.Store.AcquireSemanticSearchAuthority(ctx, profile, binding, owner, at, duration, scope)
	backend.mutate(scope, sourceFenceAcquire)
	return authority, err
}

func (backend *sourceFenceRecordingBackend) ResolveSemanticCandidates(ctx context.Context, profile, binding string,
	inputKind document.EmbeddingInputKind, vectorSpace, sourceManifest string, neighbors []vectorindex.Neighbor,
	limit int, scope store.SearchOptions,
) (store.SemanticSearchResolution, error) {
	backend.record(scope, sourceFenceResolve)
	resolution, err := backend.Store.ResolveSemanticCandidates(ctx, profile, binding, inputKind, vectorSpace,
		sourceManifest, neighbors, limit, scope)
	backend.mutate(scope, sourceFenceResolve)
	return resolution, err
}

func (backend *sourceFenceRecordingBackend) RevalidateSearchCandidates(ctx context.Context,
	candidates []store.SearchCandidateIdentity, scope store.SearchOptions, semanticProfileFingerprint,
	semanticBindingID string,
) (store.SearchCandidateRevalidation, error) {
	backend.record(scope, sourceFenceRevalidate)
	revalidation, err := backend.Store.RevalidateSearchCandidates(ctx, candidates, scope,
		semanticProfileFingerprint, semanticBindingID)
	backend.mutate(scope, sourceFenceRevalidate)
	return revalidation, err
}

var _ Backend = (*sourceFenceRecordingBackend)(nil)
var _ SemanticBackend = (*sourceFenceRecordingBackend)(nil)
var _ CandidateRevalidationBackend = (*sourceFenceRecordingBackend)(nil)
