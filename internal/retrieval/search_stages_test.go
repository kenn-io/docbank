package retrieval

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

func TestSearcherExpansionUsesVariantsWithoutBroadeningScope(t *testing.T) {
	t.Parallel()

	expander := &stageExpander{variants: []string{"zeta", "alpha"}}
	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
		config.Expansion = ExpansionConfig{Enabled: true,
			Profile: ExpansionProfile{ID: "expansion", MaxVariants: 2}, Provider: expander,
			Authorizer: &stageAuthorizer{}, Deadline: time.Second, FailurePolicy: ProviderFailureDegrade}
	})
	scope := store.SearchOptions{TagID: "tag", MIMEType: "text/plain", UnderNodeID: 7,
		ModifiedSince: "2026-01-01T00:00:00Z", ModifiedBefore: "2026-12-31T00:00:00Z"}

	report, err := searcher.Search(t.Context(), Query{Text: "original", Mode: ModeLexical, Limit: 2, Scope: scope})
	require.NoError(t, err)
	assert.Equal(t, 1, expander.calls)
	assert.Equal(t, []string{"original", "alpha", "zeta"}, backend.queries)
	assert.Equal(t, []store.SearchOptions{scope, scope, scope}, backend.scopes)
	assert.LessOrEqual(t, len(report.Results), 2)
	assert.Equal(t, []ProviderReceipt{{Stage: ProviderStageExpansion,
		Outcome: ProviderOutcomeApplied, VariantCount: 2}}, report.Receipts)
}

func TestSearcherOptionalProviderStagesAreDisabledByDefault(t *testing.T) {
	t.Parallel()

	expander := &stageExpander{variants: []string{"replacement"}}
	reranker := &stageReranker{}
	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
		config.Expansion = ExpansionConfig{Profile: ExpansionProfile{ID: "expansion", MaxVariants: 1},
			Provider: expander, Authorizer: &stageAuthorizer{}, Deadline: time.Second,
			FailurePolicy: ProviderFailureDegrade}
		config.Reranking = RerankingConfig{Profile: RerankingProfile{ID: "reranking", MaxCandidates: 1},
			Provider: reranker, Authorizer: &stageAuthorizer{}, Deadline: time.Second,
			FailurePolicy: ProviderFailureDegrade}
	})

	report, err := searcher.Search(t.Context(), Query{Text: "original", Mode: ModeLexical, Limit: 1})
	require.NoError(t, err)
	assert.Zero(t, expander.calls)
	assert.Zero(t, reranker.calls)
	assert.Equal(t, []string{"original"}, backend.queries)
	assert.Empty(t, report.Receipts)
	assert.Empty(t, report.Degradations)
	assert.Equal(t, 1, backend.revalidationCalls, "the E9 final fence remains unconditional")
}

func TestSearcherExpansionAuthorizesBeforeProviderAndDegradesSafely(t *testing.T) {
	t.Parallel()

	denied := &stageAuthorizer{err: errors.New("private query: denied")}
	expander := &stageExpander{variants: []string{"replacement"}}
	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
		config.Expansion = ExpansionConfig{Enabled: true,
			Profile:  ExpansionProfile{ID: "expansion", MaxVariants: 1},
			Provider: expander, Authorizer: denied, Deadline: time.Second,
			FailurePolicy: ProviderFailureDegrade}
	})

	report, err := searcher.Search(t.Context(), Query{Text: "private query", Mode: ModeLexical, Limit: 1})
	require.NoError(t, err)
	assert.Zero(t, expander.calls)
	assert.Equal(t, []Degradation{DegradationExpansionDegraded}, report.Degradations)
	require.Equal(t, []ProviderReceipt{{Stage: ProviderStageExpansion,
		Outcome: ProviderOutcomeAuthorizationDenied}}, report.Receipts)
	assert.NotContains(t, fmt.Sprintf("%#v", report), "private query")
	assert.Equal(t, 1, backend.revalidationCalls)
}

func TestSearcherRerankingTimesOutAndReportsNamedDegradation(t *testing.T) {
	t.Parallel()

	reranker := &stageReranker{wait: true}
	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
		config.Reranking = RerankingConfig{Enabled: true,
			Profile: RerankingProfile{ID: "reranking", MaxCandidates: 1}, Provider: reranker,
			Authorizer: &stageAuthorizer{}, Deadline: time.Millisecond, FailurePolicy: ProviderFailureDegrade}
	})

	report, err := searcher.Search(t.Context(), Query{Text: "private query", Mode: ModeLexical, Limit: 1})
	require.NoError(t, err)
	assert.Equal(t, []Degradation{DegradationRerankingDegraded}, report.Degradations)
	assert.Equal(t, []ProviderReceipt{{Stage: ProviderStageReranking,
		Outcome: ProviderOutcomeTimedOut, CandidateCount: 1}}, report.Receipts)
	assert.NotContains(t, fmt.Sprintf("%#v", report), "private query")
	assert.Equal(t, 3, backend.revalidationCalls,
		"reranking input is checked before and after authorization, then the final result is fenced")
}

func TestSearcherRejectsExpansionResponseReturnedAfterDeadline(t *testing.T) {
	t.Parallel()

	expander := &stageExpander{variants: []string{"replacement"}, waitThenSucceed: true}
	searcher, _ := stageSearcher(t, func(config *SearcherConfig) {
		config.Expansion = ExpansionConfig{Enabled: true,
			Profile: ExpansionProfile{ID: "expansion", MaxVariants: 1}, Provider: expander,
			Authorizer: &stageAuthorizer{}, Deadline: time.Millisecond, FailurePolicy: ProviderFailureDegrade}
	})

	report, err := searcher.Search(t.Context(), Query{Text: "private query", Mode: ModeLexical, Limit: 1})
	require.NoError(t, err)
	assert.Equal(t, []Degradation{DegradationExpansionDegraded}, report.Degradations)
	assert.Equal(t, []ProviderReceipt{{Stage: ProviderStageExpansion,
		Outcome: ProviderOutcomeTimedOut}}, report.Receipts)
}

func TestSearcherFailClosedProviderFailureIsSanitized(t *testing.T) {
	t.Parallel()

	searcher, _ := stageSearcher(t, func(config *SearcherConfig) {
		config.Expansion = ExpansionConfig{Enabled: true,
			Profile:    ExpansionProfile{ID: "expansion", MaxVariants: 1},
			Provider:   &stageExpander{err: errors.New("private query; provider body; credential")},
			Authorizer: &stageAuthorizer{}, Deadline: time.Second, FailurePolicy: ProviderFailureFailClosed}
	})

	_, err := searcher.Search(t.Context(), Query{Text: "private query", Mode: ModeLexical, Limit: 1})
	require.ErrorIs(t, err, ErrQueryExpansionFailed)
	assert.Equal(t, ErrQueryExpansionFailed.Error(), err.Error())
}

func TestSearcherProviderReceiptDoesNotRetainProfileText(t *testing.T) {
	t.Parallel()

	searcher, _ := stageSearcher(t, func(config *SearcherConfig) {
		config.Expansion = ExpansionConfig{Enabled: true,
			Profile:    ExpansionProfile{ID: "credential: private-profile", MaxVariants: 1},
			Provider:   &stageExpander{err: errors.New("expanded text; document text; excerpt; provider body; credential")},
			Authorizer: &stageAuthorizer{}, Deadline: time.Second, FailurePolicy: ProviderFailureDegrade}
	})

	report, err := searcher.Search(t.Context(), Query{Text: "private query", Mode: ModeLexical, Limit: 1})
	require.NoError(t, err)
	receipt := fmt.Sprintf("%#v", report.Receipts)
	for _, sensitive := range []string{"credential: private-profile", "expanded text", "document text", "excerpt", "provider body", "credential"} {
		assert.NotContains(t, receipt, sensitive)
	}
}

func TestSearcherRerankingReordersOnlyAuthorizedCandidatesAndPreservesTail(t *testing.T) {
	t.Parallel()

	reranker := &stageReranker{scores: []RerankScore{
		{Document: stageDocument(1), Score: 1},
		{Document: stageDocument(2), Score: 2},
	}}
	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
		config.Reranking = RerankingConfig{Enabled: true,
			Profile: RerankingProfile{ID: "reranking", MaxCandidates: 2}, Provider: reranker,
			Authorizer: &stageAuthorizer{}, Deadline: time.Second, FailurePolicy: ProviderFailureFailClosed}
	})
	backend.hits = stageHits(3)

	report, err := searcher.Search(t.Context(), Query{Text: "original", Mode: ModeLexical, Limit: 3})
	require.NoError(t, err)
	require.Len(t, report.Results, 3)
	assert.Equal(t, []int64{2, 1, 3}, resultNodeIDs(report.Results))
	assert.Equal(t, []int{1, 2, 3}, resultRanks(report.Results))
	assert.Equal(t, []RerankingCandidate{
		{Document: stageDocument(1), Evidence: []EvidenceReference{stageEvidence(1)}},
		{Document: stageDocument(2), Evidence: []EvidenceReference{stageEvidence(2)}},
	}, reranker.candidates)
	assert.Equal(t, 3, backend.revalidationCalls)
}

func TestSearcherRerankingKeepsBoundedPrefixAheadOfHigherScoredTail(t *testing.T) {
	t.Parallel()

	reranker := &stageReranker{scores: []RerankScore{
		{Document: stageDocument(1), Score: -2},
		{Document: stageDocument(2), Score: -1},
	}}
	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
		config.Reranking = RerankingConfig{Enabled: true,
			Profile: RerankingProfile{ID: "reranking", MaxCandidates: 2}, Provider: reranker,
			Authorizer: &stageAuthorizer{}, Deadline: time.Second, FailurePolicy: ProviderFailureFailClosed}
	})
	backend.hits = stageHits(3)

	report, err := searcher.Search(t.Context(), Query{Text: "original", Mode: ModeLexical, Limit: 3})
	require.NoError(t, err)
	assert.Equal(t, []int64{2, 1, 3}, resultNodeIDs(report.Results))
	assert.Greater(t, report.Results[2].Score, report.Results[0].Score,
		"the untouched tail remains behind the bounded reranked prefix even when score scales differ")
}

func TestSearcherRerankingReceivesOnlyBoundedAuthorizedCandidatePayload(t *testing.T) {
	t.Parallel()

	authorizer := &stageAuthorizer{}
	reranker := &stageReranker{}
	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
		config.Reranking = RerankingConfig{Enabled: true,
			Profile: RerankingProfile{ID: "reranking", MaxCandidates: MaxCandidateLimit}, Provider: reranker,
			Authorizer: authorizer, Deadline: time.Second, FailurePolicy: ProviderFailureFailClosed}
	})
	backend.hits = []store.ExplainedLexicalCandidate{
		{Node: store.Node{ID: 1, Revision: 1, CurrentVersionID: "version-1", Name: "one"}, Path: "/one",
			EvidenceKind: "rendition_segment", BuildID: "build-1", SegmentID: "segment-1",
			Excerpt: strings.Repeat("x", maxRerankingExcerptBytes+1)},
		{Node: store.Node{ID: 2, Revision: 1, CurrentVersionID: "version-2", Name: "two"}, Path: "/two",
			EvidenceKind: "rendition_segment", BuildID: "build-2", SegmentID: "segment-2", Excerpt: "out-of-scope"},
	}
	scope := store.SearchOptions{TagID: "tag", UnderNodeID: 7}

	_, err := searcher.Search(t.Context(), Query{Text: "private query", Mode: ModeLexical, Limit: 1, Scope: scope})
	require.NoError(t, err)
	require.Len(t, reranker.candidates, 1)
	assert.Equal(t, stageDocument(1), reranker.candidates[0].Document)
	assert.Len(t, reranker.candidates[0].Excerpt, maxRerankingExcerptBytes)
	assert.Equal(t, []EvidenceReference{{Kind: "rendition_segment", VaultID: "vault", NodeID: 1,
		NodeRevision: 1, ContentVersionID: "version-1", BuildID: "build-1", SegmentID: "segment-1"}},
		reranker.candidates[0].Evidence)
	require.Len(t, authorizer.operations, 1)
	assert.Equal(t, ProviderOperation{Stage: ProviderStageReranking, ProfileID: "reranking", Scope: scope,
		InputClass: ProviderInputQueryAndExcerpt, CandidateCount: 1,
		QueryBytes: len("private query"), QueryByteLimit: maxProviderQueryBytes,
		ExcerptBytes: maxRerankingExcerptBytes, ExcerptBytesPerCandidateLimit: maxRerankingExcerptBytes,
		ExcerptBytesTotalLimit: maxRerankingExcerptBytes,
		EvidenceCount:          1, EvidencePerCandidateLimit: maxRerankingEvidenceReferences,
		EvidenceTotalLimit:             maxRerankingEvidenceReferences,
		EvidenceBytes:                  rerankingEvidenceBytes(reranker.candidates[0].Evidence[0]),
		EvidenceBytesPerCandidateLimit: maxRerankingEvidenceBytes,
		EvidenceBytesTotalLimit:        maxRerankingEvidenceBytes}, authorizer.operations[0])
}

func TestSearcherRerankingAuthorizationPrecedesProviderEgress(t *testing.T) {
	t.Parallel()

	authorizer := &stageAuthorizer{err: errors.New("denied")}
	reranker := &stageReranker{}
	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
		config.Reranking = RerankingConfig{Enabled: true,
			Profile: RerankingProfile{ID: "reranking", MaxCandidates: 1}, Provider: reranker,
			Authorizer: authorizer, Deadline: time.Second, FailurePolicy: ProviderFailureDegrade}
	})

	report, err := searcher.Search(t.Context(), Query{Text: "private query", Mode: ModeLexical, Limit: 1,
		Scope: store.SearchOptions{MIMEType: "text/plain"}})
	require.NoError(t, err)
	assert.Zero(t, reranker.calls)
	require.Len(t, authorizer.operations, 1)
	assert.Equal(t, ProviderInputQueryAndExcerpt, authorizer.operations[0].InputClass)
	assert.Equal(t, []Degradation{DegradationRerankingDegraded}, report.Degradations)
	assert.Equal(t, 2, backend.revalidationCalls,
		"authorization denial skips the post-authorization check but not the final fence")
}

func TestSearcherRerankingAuthorizationBindsPerCandidateAndTotalPayloadLimits(t *testing.T) {
	t.Parallel()

	authorizer := &stageAuthorizer{}
	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
		config.Reranking = RerankingConfig{Enabled: true,
			Profile: RerankingProfile{ID: "reranking", MaxCandidates: 2}, Provider: &stageReranker{},
			Authorizer: authorizer, Deadline: time.Second, FailurePolicy: ProviderFailureFailClosed}
	})
	backend.hits = stageHits(2)
	backend.hits[0].Excerpt = strings.Repeat("a", maxRerankingExcerptBytes+1)
	backend.hits[1].Excerpt = strings.Repeat("b", maxRerankingExcerptBytes+1)

	_, err := searcher.Search(t.Context(), Query{Text: "private query", Mode: ModeLexical, Limit: 2})
	require.NoError(t, err)
	require.Len(t, authorizer.operations, 1)
	operation := authorizer.operations[0]
	assert.Equal(t, 2*maxRerankingExcerptBytes, operation.ExcerptBytes)
	assert.Equal(t, maxRerankingExcerptBytes, operation.ExcerptBytesPerCandidateLimit)
	assert.Equal(t, 2*maxRerankingExcerptBytes, operation.ExcerptBytesTotalLimit)
	assert.Equal(t, 2, operation.EvidenceCount)
	assert.Equal(t, 2*maxRerankingEvidenceReferences, operation.EvidenceTotalLimit)
	assert.Positive(t, operation.EvidenceBytes)
	assert.Equal(t, 2*maxRerankingEvidenceBytes, operation.EvidenceBytesTotalLimit)
}

func TestSearcherRerankingFailsClosedWhenAuthorizationRevokesCandidate(t *testing.T) {
	t.Parallel()

	reranker := &stageReranker{}
	authorizer := &stageAuthorizer{}
	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
		config.Reranking = RerankingConfig{Enabled: true,
			Profile: RerankingProfile{ID: "reranking", MaxCandidates: 2}, Provider: reranker,
			Authorizer: authorizer,
			Deadline:   time.Second, FailurePolicy: ProviderFailureDegrade}
	})
	backend.hits = stageHits(2)
	authorizer.after = func() { backend.keepNodeIDs = map[int64]bool{1: true} }

	_, err := searcher.Search(t.Context(), Query{Text: "original", Mode: ModeLexical, Limit: 2})
	require.ErrorIs(t, err, ErrRerankingFailed)
	assert.Zero(t, reranker.calls)
	assert.Equal(t, 2, backend.revalidationCalls)
}

func TestSearcherRerankingPostAuthorizationFailureIsSanitized(t *testing.T) {
	t.Parallel()

	reranker := &stageReranker{}
	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
		config.Reranking = RerankingConfig{Enabled: true,
			Profile: RerankingProfile{ID: "reranking", MaxCandidates: 1}, Provider: reranker,
			Authorizer: &stageAuthorizer{}, Deadline: time.Second, FailurePolicy: ProviderFailureDegrade}
	})
	backend.revalidationCallError = map[int]error{2: errors.New("private stale authority detail")}

	_, err := searcher.Search(t.Context(), Query{Text: "private query", Mode: ModeLexical, Limit: 1})

	require.ErrorIs(t, err, ErrRerankingFailed)
	assert.Equal(t, ErrRerankingFailed.Error(), err.Error())
	assert.Zero(t, reranker.calls)
	assert.Equal(t, 2, backend.revalidationCalls)
}

func TestSearcherRerankingDoesNotDegradeAuthorityFailureAfterDeadline(t *testing.T) {
	t.Parallel()

	reranker := &stageReranker{}
	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
		config.Reranking = RerankingConfig{Enabled: true,
			Profile: RerankingProfile{ID: "reranking", MaxCandidates: 1}, Provider: reranker,
			Authorizer: &stageAuthorizer{}, Deadline: time.Millisecond, FailurePolicy: ProviderFailureDegrade}
	})
	backend.revalidationCallAction = map[int]func(context.Context) error{2: func(ctx context.Context) error {
		<-ctx.Done()
		return errors.New("private stale authority detail")
	}}

	_, err := searcher.Search(t.Context(), Query{Text: "private query", Mode: ModeLexical, Limit: 1})

	require.ErrorIs(t, err, ErrRerankingFailed)
	assert.Equal(t, ErrRerankingFailed.Error(), err.Error())
	assert.Zero(t, reranker.calls)
	assert.Equal(t, 2, backend.revalidationCalls)
}

func TestSearcherRerankingDoesNotDegradeMixedAuthorityAndDeadlineFailure(t *testing.T) {
	t.Parallel()

	reranker := &stageReranker{}
	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
		config.Reranking = RerankingConfig{Enabled: true,
			Profile: RerankingProfile{ID: "reranking", MaxCandidates: 1}, Provider: reranker,
			Authorizer: &stageAuthorizer{}, Deadline: time.Millisecond, FailurePolicy: ProviderFailureDegrade}
	})
	backend.revalidationCallAction = map[int]func(context.Context) error{2: func(ctx context.Context) error {
		<-ctx.Done()
		return errors.Join(ctx.Err(), errors.New("private stale authority detail"))
	}}

	_, err := searcher.Search(t.Context(), Query{Text: "private query", Mode: ModeLexical, Limit: 1})

	require.ErrorIs(t, err, ErrRerankingFailed)
	assert.Equal(t, ErrRerankingFailed.Error(), err.Error())
	assert.Zero(t, reranker.calls)
	assert.Equal(t, 2, backend.revalidationCalls)
}

func TestSearcherRerankingDoesNotDegradeSingleCauseJoinedDeadline(t *testing.T) {
	t.Parallel()

	reranker := &stageReranker{}
	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
		config.Reranking = RerankingConfig{Enabled: true,
			Profile: RerankingProfile{ID: "reranking", MaxCandidates: 1}, Provider: reranker,
			Authorizer: &stageAuthorizer{}, Deadline: time.Millisecond, FailurePolicy: ProviderFailureDegrade}
	})
	backend.revalidationCallAction = map[int]func(context.Context) error{2: func(ctx context.Context) error {
		<-ctx.Done()
		return errors.Join(ctx.Err())
	}}

	_, err := searcher.Search(t.Context(), Query{Text: "private query", Mode: ModeLexical, Limit: 1})

	require.ErrorIs(t, err, ErrRerankingFailed)
	assert.Equal(t, ErrRerankingFailed.Error(), err.Error())
	assert.Zero(t, reranker.calls)
	assert.Equal(t, 2, backend.revalidationCalls)
}

func TestSearcherRerankingDoesNotDegradeCustomDeadlineEquivalentFailure(t *testing.T) {
	t.Parallel()

	reranker := &stageReranker{}
	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
		config.Reranking = RerankingConfig{Enabled: true,
			Profile: RerankingProfile{ID: "reranking", MaxCandidates: 1}, Provider: reranker,
			Authorizer: &stageAuthorizer{}, Deadline: time.Millisecond, FailurePolicy: ProviderFailureDegrade}
	})
	backend.revalidationCallAction = map[int]func(context.Context) error{2: func(ctx context.Context) error {
		<-ctx.Done()
		return deadlineEquivalentError{}
	}}

	_, err := searcher.Search(t.Context(), Query{Text: "private query", Mode: ModeLexical, Limit: 1})

	require.ErrorIs(t, err, ErrRerankingFailed)
	assert.Equal(t, ErrRerankingFailed.Error(), err.Error())
	assert.Zero(t, reranker.calls)
	assert.Equal(t, 2, backend.revalidationCalls)
}

func TestSearcherRerankingDegradesActualRevalidationDeadline(t *testing.T) {
	t.Parallel()

	reranker := &stageReranker{}
	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
		config.Reranking = RerankingConfig{Enabled: true,
			Profile: RerankingProfile{ID: "reranking", MaxCandidates: 1}, Provider: reranker,
			Authorizer: &stageAuthorizer{}, Deadline: time.Millisecond, FailurePolicy: ProviderFailureDegrade}
	})
	backend.revalidationCallAction = map[int]func(context.Context) error{2: func(ctx context.Context) error {
		<-ctx.Done()
		return fmt.Errorf("revalidation interrupted: %w", ctx.Err())
	}}

	report, err := searcher.Search(t.Context(), Query{Text: "private query", Mode: ModeLexical, Limit: 1})

	require.NoError(t, err)
	assert.Equal(t, []Degradation{DegradationRerankingDegraded}, report.Degradations)
	assert.Equal(t, []ProviderReceipt{{Stage: ProviderStageReranking,
		Outcome: ProviderOutcomeTimedOut, CandidateCount: 1}}, report.Receipts)
	assert.Zero(t, reranker.calls)
	assert.Equal(t, 3, backend.revalidationCalls)
}

func TestSearcherRerankingPreservesParentCancellationDuringRevalidation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	reranker := &stageReranker{}
	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
		config.Reranking = RerankingConfig{Enabled: true,
			Profile: RerankingProfile{ID: "reranking", MaxCandidates: 1}, Provider: reranker,
			Authorizer: &stageAuthorizer{}, Deadline: time.Second, FailurePolicy: ProviderFailureDegrade}
	})
	backend.revalidationCallAction = map[int]func(context.Context) error{2: func(context.Context) error {
		cancel()
		return errors.New("private stale authority detail")
	}}

	_, err := searcher.Search(ctx, Query{Text: "private query", Mode: ModeLexical, Limit: 1})

	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, reranker.calls)
	assert.Equal(t, 2, backend.revalidationCalls)
}

func TestSearcherOptionalProviderDegradationDoesNotHideFinalAuthorityFailure(t *testing.T) {
	t.Parallel()

	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
		config.Reranking = RerankingConfig{Enabled: true,
			Profile:    RerankingProfile{ID: "reranking", MaxCandidates: 1},
			Provider:   &stageReranker{err: errors.New("provider unavailable")},
			Authorizer: &stageAuthorizer{}, Deadline: time.Second, FailurePolicy: ProviderFailureDegrade}
	})
	backend.revalidationCallError = map[int]error{3: store.ErrVectorIndexSourceStale}

	_, err := searcher.Search(t.Context(), Query{Text: "query", Mode: ModeLexical, Limit: 1})

	require.ErrorIs(t, err, store.ErrVectorIndexSourceStale)
}

func TestSearcherRerankingDoesNotHidePreAuthorizationAuthorityFailure(t *testing.T) {
	t.Parallel()

	reranker := &stageReranker{}
	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
		config.Reranking = RerankingConfig{Enabled: true,
			Profile: RerankingProfile{ID: "reranking", MaxCandidates: 1}, Provider: reranker,
			Authorizer: &stageAuthorizer{}, Deadline: time.Second, FailurePolicy: ProviderFailureDegrade}
	})
	backend.revalidationCallError = map[int]error{1: store.ErrVectorIndexSourceStale}

	_, err := searcher.Search(t.Context(), Query{Text: "query", Mode: ModeLexical, Limit: 1})

	require.ErrorIs(t, err, store.ErrVectorIndexSourceStale)
	assert.Zero(t, reranker.calls)
}

func TestSearcherExpandedVariantsRevalidateCurrentScopeBeforeReranking(t *testing.T) {
	t.Parallel()

	expander := &stageExpander{variants: []string{"expanded"}}
	reranker := &stageReranker{}
	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
		config.Expansion = ExpansionConfig{Enabled: true,
			Profile: ExpansionProfile{ID: "expansion", MaxVariants: 1}, Provider: expander,
			Authorizer: &stageAuthorizer{}, Deadline: time.Second, FailurePolicy: ProviderFailureFailClosed}
		config.Reranking = RerankingConfig{Enabled: true,
			Profile: RerankingProfile{ID: "reranking", MaxCandidates: 2}, Provider: reranker,
			Authorizer: &stageAuthorizer{}, Deadline: time.Second, FailurePolicy: ProviderFailureFailClosed}
	})
	backend.hits = stageHits(2)
	backend.keepNodeIDs = map[int64]bool{1: true}

	report, err := searcher.Search(t.Context(), Query{Text: "original", Mode: ModeLexical, Limit: 2,
		Scope: store.SearchOptions{TagID: "tag"}})
	require.NoError(t, err)
	assert.Len(t, report.Results, 1)
	assert.Len(t, reranker.candidates, 1)
	assert.Equal(t, int64(1), reranker.candidates[0].Document.NodeID)
	assert.Equal(t, 3, backend.revalidationCalls)
}

func TestSearcherSanitizesExpandedSearchFailure(t *testing.T) {
	t.Parallel()

	expander := &stageExpander{variants: []string{"expanded secret"}}
	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
		config.Expansion = ExpansionConfig{Enabled: true,
			Profile: ExpansionProfile{ID: "expansion", MaxVariants: 1}, Provider: expander,
			Authorizer: &stageAuthorizer{}, Deadline: time.Second, FailurePolicy: ProviderFailureFailClosed}
	})
	backend.errForQuery = map[string]error{"expanded secret": errors.New("original secret / expanded secret")}

	_, err := searcher.Search(t.Context(), Query{Text: "original secret", Mode: ModeLexical, Limit: 1})
	require.ErrorIs(t, err, ErrExpandedSearchFailed)
	assert.Equal(t, ErrExpandedSearchFailed.Error(), err.Error())
}

func TestSearcherExpandedSearchFailureDoesNotBecomeSuccessfulDegradation(t *testing.T) {
	t.Parallel()

	expander := &stageExpander{variants: []string{"expanded"}}
	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
		config.Expansion = ExpansionConfig{Enabled: true,
			Profile: ExpansionProfile{ID: "expansion", MaxVariants: 1}, Provider: expander,
			Authorizer: &stageAuthorizer{}, Deadline: time.Second, FailurePolicy: ProviderFailureDegrade}
	})
	backend.errForQuery = map[string]error{"expanded": store.ErrVectorIndexSourceStale}

	_, err := searcher.Search(t.Context(), Query{Text: "original", Mode: ModeLexical, Limit: 1})

	require.ErrorIs(t, err, ErrExpandedSearchFailed)
}

func TestSearcherExpansionDoesNotHideBaseAuthorityFailure(t *testing.T) {
	t.Parallel()

	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
		config.Expansion = ExpansionConfig{Enabled: true,
			Profile:  ExpansionProfile{ID: "expansion", MaxVariants: 1},
			Provider: &stageExpander{variants: []string{"expanded"}}, Authorizer: &stageAuthorizer{},
			Deadline: time.Second, FailurePolicy: ProviderFailureDegrade}
	})
	backend.errForQuery = map[string]error{"original": store.ErrVectorIndexSourceStale}

	_, err := searcher.Search(t.Context(), Query{Text: "original", Mode: ModeLexical, Limit: 1})

	require.ErrorIs(t, err, store.ErrVectorIndexSourceStale)
}

func TestSearcherPreservesParentCancellationDuringExpandedSearch(t *testing.T) {
	t.Parallel()

	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
		config.Expansion = ExpansionConfig{Enabled: true,
			Profile:  ExpansionProfile{ID: "expansion", MaxVariants: 1},
			Provider: &stageExpander{variants: []string{"expanded"}}, Authorizer: &stageAuthorizer{},
			Deadline: time.Second, FailurePolicy: ProviderFailureFailClosed}
	})
	ctx, cancel := context.WithCancel(t.Context())
	backend.cancelForQuery, backend.cancel = "expanded", cancel

	_, err := searcher.Search(ctx, Query{Text: "original", Mode: ModeLexical, Limit: 1})

	require.ErrorIs(t, err, context.Canceled)
}

func TestSearcherBoundsRerankingToTheRequestedCandidateLimit(t *testing.T) {
	t.Parallel()

	reranker := &stageReranker{}
	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
		config.Reranking = RerankingConfig{Enabled: true,
			Profile: RerankingProfile{ID: "reranking", MaxCandidates: MaxCandidateLimit}, Provider: reranker,
			Authorizer: &stageAuthorizer{}, Deadline: time.Second, FailurePolicy: ProviderFailureFailClosed}
	})
	backend.hits = stageHits(2)

	report, err := searcher.Search(t.Context(), Query{Text: "original", Mode: ModeLexical, Limit: 1})
	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	assert.Equal(t, []RerankingCandidate{{Document: stageDocument(1),
		Evidence: []EvidenceReference{stageEvidence(1)}}}, reranker.candidates)
}

func TestSearcherBoundsSemanticCandidatesToTheRequestedLimit(t *testing.T) {
	t.Parallel()

	searcher, backend, _, descriptor := retrievalSearcherFixture(t, true, 1)
	backend.semantic = append(backend.semantic, store.SemanticSearchCandidate{VaultID: "vault", NodeID: 9,
		NodeRevision: 1, ContentVersionID: "version-extra", Path: "/extra",
		VectorSpaceID: backend.authority.VectorSpace.ID, EmbeddingSetID: strings.Repeat("f", 64),
		InputGenerationID: strings.Repeat("1", 64), InputID: "input-extra",
		InputKind: document.EmbeddingInputOriginalFile, Score: 0.5})

	report, err := searcher.Search(t.Context(), Query{Text: "original", Mode: ModeSemantic, Limit: 1,
		ProcessingProfileFingerprint: strings.Repeat("a", 64), BindingID: "required",
		Authorization: retrievalAuthorization(descriptor)})
	require.NoError(t, err)
	assert.Len(t, report.Results, 1)
	assert.True(t, report.Truncated)
}

func TestMergeVariantReportsSumsOnlyReciprocalRankContributionsAndDeduplicates(t *testing.T) {
	t.Parallel()

	shared := stageResult(2, 1, 999)
	shared.Explanation = []Contribution{{Lane: LaneLexical, Rank: 1, Contribution: 1.0 / 61.0}}
	variant := stageResult(2, 2, -999)
	variant.Explanation = []Contribution{{Lane: LaneLexical, Rank: 2, Contribution: 1.0 / 62.0}}

	merged, err := mergeVariantReports([]Report{
		stageReport(shared), stageReport(variant),
	}, 2)
	require.NoError(t, err)
	require.Len(t, merged.Results, 1)
	assert.Equal(t, int64(2), merged.Results[0].Document.NodeID)
	assert.InDelta(t, 1.0/61.0+1.0/62.0, merged.Results[0].Score, 1e-12)
	assert.Len(t, merged.Results[0].Explanation, 2)
}

func TestMergeVariantReportsOrdersDeterministicallyAndTruncatesAtRequestedBound(t *testing.T) {
	t.Parallel()

	merged, err := mergeVariantReports([]Report{
		stageReport(stageResult(3, 1, 42), stageResult(2, 2, 41)),
		stageReport(stageResult(1, 1, 40)),
	}, 2)
	require.NoError(t, err)
	assert.True(t, merged.Truncated)
	require.Len(t, merged.Results, 2)
	assert.Equal(t, []int64{1, 3}, resultNodeIDs(merged.Results),
		"equal contribution scores are broken by stable document identity")
	assert.Equal(t, []int{1, 2}, resultRanks(merged.Results))
}

func TestMergeVariantReportsConservativelyAggregatesCoverage(t *testing.T) {
	t.Parallel()

	first := Report{RequestedMode: ModeSemantic, ActualMode: ModeSemantic,
		Coverage: Coverage{BindingRequired: false, ScopedDocuments: 1, CompleteDocuments: 1, State: CoverageComplete}}
	second := Report{RequestedMode: ModeSemantic, ActualMode: ModeSemantic,
		Coverage: Coverage{BindingRequired: true, ScopedDocuments: 2, CompleteDocuments: 1, State: CoverageIncomplete}}

	merged, err := mergeVariantReports([]Report{first, second}, 1)
	require.NoError(t, err)
	assert.Equal(t, Coverage{BindingRequired: true, ScopedDocuments: 2, CompleteDocuments: 1,
		State: CoverageIncomplete}, merged.Coverage)

	unknown, err := mergeVariantReports([]Report{first, {
		RequestedMode: ModeSemantic, ActualMode: ModeSemantic, Coverage: Coverage{State: CoverageUnknown},
	}}, 1)
	require.NoError(t, err)
	assert.Equal(t, Coverage{State: CoverageUnknown}, unknown.Coverage)
}

func TestMergeVariantReportsRejectsIncompatibleModesAndDegradations(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		reports []Report
	}{
		{name: "requested mode", reports: []Report{
			{RequestedMode: ModeLexical, ActualMode: ModeLexical},
			{RequestedMode: ModeAuto, ActualMode: ModeLexical},
		}},
		{name: "actual mode", reports: []Report{
			{RequestedMode: ModeAuto, ActualMode: ModeLexical},
			{RequestedMode: ModeAuto, ActualMode: ModeHybrid},
		}},
		{name: "primary degradation", reports: []Report{
			{RequestedMode: ModeAuto, ActualMode: ModeLexical, Degradation: DegradationIncompleteCoverage},
			{RequestedMode: ModeAuto, ActualMode: ModeLexical, Degradation: DegradationProviderUnavailable},
		}},
		{name: "stage degradations", reports: []Report{
			{RequestedMode: ModeLexical, ActualMode: ModeLexical},
			{RequestedMode: ModeLexical, ActualMode: ModeLexical,
				Degradations: []Degradation{DegradationExpansionDegraded}},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := mergeVariantReports(test.reports, 1)
			require.Error(t, err)
		})
	}
}

func TestMergeVariantReportsRejectsMixedAuthorityForOneDocument(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		mutate func(*Result)
	}{
		{name: "path", mutate: func(result *Result) { result.Path = "/moved/one" }},
		{name: "node revision", mutate: func(result *Result) { result.Evidence[0].NodeRevision++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			first := stageResult(1, 1, 1)
			second := stageResult(1, 1, 1)
			test.mutate(&second)

			_, err := mergeVariantReports([]Report{stageReport(first), stageReport(second)}, 2)

			require.ErrorContains(t, err, "authority")
		})
	}
}

func TestExpandedAutoSearchFailsClosedWhenRequiredCoverageChangesAtFinalFence(t *testing.T) {
	t.Parallel()

	searcher, backend := stageSearcher(t, func(*SearcherConfig) {})
	backend.revalidationCoverage = &store.SearchCoverageSnapshot{ScopedDocuments: 2, CompleteDocuments: 1}
	report := Report{RequestedMode: ModeAuto, ActualMode: ModeHybrid,
		Coverage: Coverage{BindingRequired: true, ScopedDocuments: 1, CompleteDocuments: 1,
			State: CoverageComplete}, Results: []Result{stageResult(1, 1, 1)}}

	_, err := searcher.revalidateReport(t.Context(), Query{
		ProcessingProfileFingerprint: "profile", BindingID: "required"}, report)

	require.ErrorContains(t, err, "required semantic coverage changed")
}

type stageBackend struct {
	queries                []string
	scopes                 []store.SearchOptions
	hits                   []store.ExplainedLexicalCandidate
	errForQuery            map[string]error
	cancelForQuery         string
	cancel                 context.CancelFunc
	keepNodeIDs            map[int64]bool
	revalidationCoverage   *store.SearchCoverageSnapshot
	revalidationCalls      int
	revalidationRequests   [][]store.SearchCandidateIdentity
	revalidationCallError  map[int]error
	revalidationCallAction map[int]func(context.Context) error
}

type deadlineEquivalentError struct{}

func (deadlineEquivalentError) Error() string { return "private deadline-equivalent detail" }

func (deadlineEquivalentError) Is(target error) bool { return target == context.DeadlineExceeded }

func (backend *stageBackend) VaultID() string { return "vault" }

func (backend *stageBackend) SearchExplainedLexicalCandidates(_ context.Context, query string, _ int,
	scope store.SearchOptions,
) ([]store.ExplainedLexicalCandidate, bool, error) {
	if query == backend.cancelForQuery {
		backend.cancel()
		return nil, false, context.Canceled
	}
	if err := backend.errForQuery[query]; err != nil {
		return nil, false, err
	}
	backend.queries = append(backend.queries, query)
	backend.scopes = append(backend.scopes, scope)
	hits := backend.hits
	if hits == nil {
		hits = stageHits(2)
	}
	return slices.Clone(hits), false, nil
}

func (backend *stageBackend) RevalidateSearchCandidates(ctx context.Context,
	candidates []store.SearchCandidateIdentity, _ store.SearchOptions, _, _ string,
) (store.SearchCandidateRevalidation, error) {
	backend.revalidationCalls++
	backend.revalidationRequests = append(backend.revalidationRequests, slices.Clone(candidates))
	for _, candidate := range candidates {
		if candidate.NodeID <= 0 || candidate.NodeRevision <= 0 || candidate.ContentVersionID == "" ||
			candidate.PathChecksum == "" || len(candidate.Evidence) == 0 {
			return store.SearchCandidateRevalidation{}, errors.New("stage fixture received incomplete candidate identity")
		}
		for _, evidence := range candidate.Evidence {
			if evidence.Kind == "" {
				return store.SearchCandidateRevalidation{}, errors.New("stage fixture received incomplete evidence identity")
			}
		}
	}
	if action := backend.revalidationCallAction[backend.revalidationCalls]; action != nil {
		if err := action(ctx); err != nil {
			return store.SearchCandidateRevalidation{}, err
		}
	}
	if err := backend.revalidationCallError[backend.revalidationCalls]; err != nil {
		return store.SearchCandidateRevalidation{}, err
	}
	kept := candidates
	if backend.keepNodeIDs != nil {
		kept = nil
		for _, candidate := range candidates {
			if backend.keepNodeIDs[candidate.NodeID] {
				kept = append(kept, candidate)
			}
		}
	}
	return store.SearchCandidateRevalidation{Candidates: kept, Coverage: backend.revalidationCoverage}, nil
}

func stageSearcher(t *testing.T, configure func(*SearcherConfig)) (*Searcher, *stageBackend) {
	t.Helper()
	backend := &stageBackend{}
	config := SearcherConfig{Backend: backend, Owner: "retrieval-test", LeaseDuration: time.Minute, Clock: time.Now}
	configure(&config)
	searcher, err := NewSearcher(config)
	require.NoError(t, err)
	return searcher, backend
}

type stageAuthorizer struct {
	err        error
	after      func()
	operations []ProviderOperation
}

func (authorizer *stageAuthorizer) AuthorizeExpansion(_ context.Context, operation ProviderOperation) error {
	authorizer.operations = append(authorizer.operations, operation)
	if authorizer.after != nil {
		authorizer.after()
	}
	return authorizer.err
}

func (authorizer *stageAuthorizer) AuthorizeReranking(_ context.Context, operation ProviderOperation) error {
	authorizer.operations = append(authorizer.operations, operation)
	if authorizer.after != nil {
		authorizer.after()
	}
	return authorizer.err
}

type stageExpander struct {
	variants        []string
	err             error
	waitThenSucceed bool
	after           func()
	calls           int
}

func (provider *stageExpander) Expand(ctx context.Context, _ ExpansionRequest) ([]string, error) {
	provider.calls++
	if provider.after != nil {
		provider.after()
	}
	if provider.waitThenSucceed {
		<-ctx.Done()
		return provider.variants, nil
	}
	return provider.variants, provider.err
}

type stageReranker struct {
	scores     []RerankScore
	candidates []RerankingCandidate
	err        error
	wait       bool
	after      func()
	calls      int
}

func (provider *stageReranker) Rerank(ctx context.Context, request RerankingRequest) ([]RerankScore, error) {
	provider.calls++
	provider.candidates = slices.Clone(request.Candidates)
	if provider.after != nil {
		provider.after()
	}
	if provider.wait {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if provider.err != nil {
		return nil, provider.err
	}
	if provider.scores != nil {
		return slices.Clone(provider.scores), nil
	}
	scores := make([]RerankScore, len(request.Candidates))
	for index, candidate := range request.Candidates {
		scores[index] = RerankScore{Document: candidate.Document, Score: float64(len(scores) - index)}
	}
	return scores, nil
}

func stageHits(count int) []store.ExplainedLexicalCandidate {
	hits := make([]store.ExplainedLexicalCandidate, count)
	for index := range hits {
		id := int64(index + 1)
		hits[index] = store.ExplainedLexicalCandidate{Node: store.Node{ID: id, Revision: 1,
			CurrentVersionID: fmt.Sprintf("version-%d", id), Name: fmt.Sprintf("node-%d", id)},
			Path: fmt.Sprintf("/node-%d", id), EvidenceKind: "node_name"}
	}
	return hits
}

func stageDocument(id int64) DocumentIdentity {
	return DocumentIdentity{VaultID: "vault", NodeID: id, ContentVersionID: fmt.Sprintf("version-%d", id)}
}

func stageEvidence(id int64) EvidenceReference {
	return EvidenceReference{Kind: "node_name", VaultID: "vault", NodeID: id,
		NodeRevision: 1, ContentVersionID: fmt.Sprintf("version-%d", id)}
}

func stageResult(id int64, laneRank int, score float64) Result {
	contribution := 1 / float64(ReciprocalRankK+laneRank)
	return Result{Document: stageDocument(id), Rank: laneRank, Score: score,
		Path: fmt.Sprintf("/node-%d", id), LexicalRank: laneRank,
		Evidence:    []EvidenceReference{stageEvidence(id)},
		Explanation: []Contribution{{Lane: LaneLexical, Rank: laneRank, Contribution: contribution}}}
}

func stageReport(results ...Result) Report {
	return Report{RequestedMode: ModeLexical, ActualMode: ModeLexical,
		Coverage: Coverage{State: CoverageUnknown}, Results: results}
}

func resultNodeIDs(results []Result) []int64 {
	ids := make([]int64, len(results))
	for index, result := range results {
		ids[index] = result.Document.NodeID
	}
	return ids
}

func resultRanks(results []Result) []int {
	ranks := make([]int, len(results))
	for index, result := range results {
		ranks[index] = result.Rank
	}
	return ranks
}
