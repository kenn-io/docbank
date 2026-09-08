package retrieval

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func TestValidateExpansionVariantsRejectsMalformedProviderOutput(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		variants []string
	}{
		{name: "empty", variants: []string{" "}},
		{name: "duplicate", variants: []string{"expanded", "expanded"}},
		{name: "original query", variants: []string{"original"}},
		{name: "over limit", variants: []string{"one", "two", "three"}},
		{name: "oversized text", variants: []string{strings.Repeat("x", maxProviderQueryBytes+1)}},
		{name: "invalid UTF-8", variants: []string{string([]byte{0xff})}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := validateExpansionVariants("original", test.variants, 2)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "original")
			assert.NotContains(t, err.Error(), "expanded")
		})
	}
}

func TestValidateExpansionVariantsSortsBoundedVariants(t *testing.T) {
	t.Parallel()

	variants, err := validateExpansionVariants("original", []string{"zeta", "alpha"}, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"alpha", "zeta"}, variants)
}

func TestValidateRerankingScoresRequiresOneFiniteScoreForEveryAllowedCandidate(t *testing.T) {
	t.Parallel()

	first := DocumentIdentity{VaultID: "vault", NodeID: 1, ContentVersionID: "version-one"}
	second := DocumentIdentity{VaultID: "vault", NodeID: 2, ContentVersionID: "version-two"}
	unknown := DocumentIdentity{VaultID: "vault", NodeID: 3, ContentVersionID: "version-three"}
	for _, test := range []struct {
		name   string
		scores []RerankScore
	}{
		{name: "missing", scores: []RerankScore{{Document: first, Score: 1}}},
		{name: "duplicate", scores: []RerankScore{{Document: first, Score: 1}, {Document: first, Score: 2}}},
		{name: "unknown", scores: []RerankScore{{Document: first, Score: 1}, {Document: unknown, Score: 2}}},
		{name: "not a number", scores: []RerankScore{{Document: first, Score: 1}, {Document: second, Score: math.NaN()}}},
		{name: "infinite", scores: []RerankScore{{Document: first, Score: 1}, {Document: second, Score: math.Inf(1)}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validateRerankScores([]DocumentIdentity{first, second}, test.scores)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "version-one")
			assert.NotContains(t, err.Error(), "version-two")
		})
	}
}

func TestProviderConfigRejectsEveryMissingEnabledExpansionDependency(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		mutate func(*ExpansionConfig)
	}{
		{name: "profile ID", mutate: func(config *ExpansionConfig) { config.Profile.ID = "" }},
		{name: "positive variant limit", mutate: func(config *ExpansionConfig) { config.Profile.MaxVariants = 0 }},
		{name: "bounded variant limit", mutate: func(config *ExpansionConfig) {
			config.Profile.MaxVariants = maxQueryExpansionVariants + 1
		}},
		{name: "provider", mutate: func(config *ExpansionConfig) { config.Provider = nil }},
		{name: "authorizer", mutate: func(config *ExpansionConfig) { config.Authorizer = nil }},
		{name: "positive deadline", mutate: func(config *ExpansionConfig) { config.Deadline = 0 }},
		{name: "valid policy", mutate: func(config *ExpansionConfig) { config.FailurePolicy = "invalid" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := validExpansionConfig()
			test.mutate(&config)
			_, err := NewSearcher(providerSearcherConfig(config, RerankingConfig{}))
			require.EqualError(t, err, "query expansion configuration is invalid")
		})
	}
}

func TestProviderConfigRejectsEveryMissingEnabledRerankingDependency(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		mutate func(*RerankingConfig)
	}{
		{name: "profile ID", mutate: func(config *RerankingConfig) { config.Profile.ID = "" }},
		{name: "positive candidate limit", mutate: func(config *RerankingConfig) { config.Profile.MaxCandidates = 0 }},
		{name: "bounded candidate limit", mutate: func(config *RerankingConfig) {
			config.Profile.MaxCandidates = MaxCandidateLimit + 1
		}},
		{name: "provider", mutate: func(config *RerankingConfig) { config.Provider = nil }},
		{name: "authorizer", mutate: func(config *RerankingConfig) { config.Authorizer = nil }},
		{name: "positive deadline", mutate: func(config *RerankingConfig) { config.Deadline = 0 }},
		{name: "valid policy", mutate: func(config *RerankingConfig) { config.FailurePolicy = "invalid" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := validRerankingConfig()
			test.mutate(&config)
			_, err := NewSearcher(providerSearcherConfig(ExpansionConfig{}, config))
			require.EqualError(t, err, "reranking configuration is invalid")
		})
	}
}

func TestProviderConfigDisabledStagesNeedNoDependencies(t *testing.T) {
	t.Parallel()

	searcher, err := NewSearcher(providerSearcherConfig(ExpansionConfig{}, RerankingConfig{}))

	require.NoError(t, err)
	assert.False(t, searcher.expansion.Enabled)
	assert.False(t, searcher.reranking.Enabled)
}

func TestProviderStageDisabledMakesNoCalls(t *testing.T) {
	t.Parallel()

	searcher := &Searcher{
		expansion: ExpansionConfig{
			Provider: queryExpansionProviderFunc(func(context.Context, ExpansionRequest) ([]string, error) {
				t.Fatal("disabled expansion called provider")
				return nil, nil
			}),
			Authorizer: expansionAuthorizerFunc(func(context.Context, ProviderOperation) error {
				t.Fatal("disabled expansion called authorizer")
				return nil
			}),
		},
		reranking: RerankingConfig{
			Provider: rerankingProviderFunc(func(context.Context, RerankingRequest) ([]RerankScore, error) {
				t.Fatal("disabled reranking called provider")
				return nil, nil
			}),
			Authorizer: rerankingAuthorizerFunc(func(context.Context, ProviderOperation) error {
				t.Fatal("disabled reranking called authorizer")
				return nil
			}),
		},
	}
	variants, expansionReceipt, expansionDegradation, err := searcher.expand(t.Context(), Query{Text: "private query"})
	require.NoError(t, err)
	assert.Nil(t, variants)
	assert.Nil(t, expansionReceipt)
	assert.Equal(t, DegradationNone, expansionDegradation)

	report := providerReportFixture()
	got, rerankingReceipt, rerankingDegradation, err := searcher.rerank(t.Context(), Query{Text: "private query"}, report)
	require.NoError(t, err)
	assert.Equal(t, report, got)
	assert.Nil(t, rerankingReceipt)
	assert.Equal(t, DegradationNone, rerankingDegradation)
}

func TestProviderStageAuthorizationDenialPrecedesEgress(t *testing.T) {
	t.Parallel()

	t.Run("expansion", func(t *testing.T) {
		t.Parallel()
		providerCalls := 0
		config := validExpansionConfig()
		config.Authorizer = expansionAuthorizerFunc(func(context.Context, ProviderOperation) error {
			return errors.New("private authorization detail")
		})
		config.Provider = queryExpansionProviderFunc(func(context.Context, ExpansionRequest) ([]string, error) {
			providerCalls++
			return []string{"egress happened"}, nil
		})
		variants, receipt, degradation, err := (&Searcher{expansion: config}).expand(t.Context(), Query{Text: "private query"})
		require.NoError(t, err)
		assert.Nil(t, variants)
		assert.Equal(t, 0, providerCalls)
		assert.Equal(t, &ProviderReceipt{Stage: ProviderStageExpansion,
			Outcome: ProviderOutcomeAuthorizationDenied}, receipt)
		assert.Equal(t, DegradationExpansionDegraded, degradation)
	})

	t.Run("reranking", func(t *testing.T) {
		t.Parallel()
		providerCalls := 0
		config := validRerankingConfig()
		config.Authorizer = rerankingAuthorizerFunc(func(context.Context, ProviderOperation) error {
			return errors.New("private authorization detail")
		})
		config.Provider = rerankingProviderFunc(func(context.Context, RerankingRequest) ([]RerankScore, error) {
			providerCalls++
			return nil, nil
		})
		report := providerReportFixture()
		got, receipt, degradation, err := (&Searcher{reranking: config}).rerank(t.Context(), Query{Text: "private query"}, report)
		require.NoError(t, err)
		assert.Equal(t, report, got)
		assert.Equal(t, 0, providerCalls)
		assert.Equal(t, &ProviderReceipt{Stage: ProviderStageReranking,
			Outcome: ProviderOutcomeAuthorizationDenied, CandidateCount: 2}, receipt)
		assert.Equal(t, DegradationRerankingDegraded, degradation)
	})
}

func TestProviderStageParentCancellationSurvives(t *testing.T) {
	t.Parallel()

	t.Run("expansion", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		config := validExpansionConfig()
		config.Provider = queryExpansionProviderFunc(func(context.Context, ExpansionRequest) ([]string, error) {
			cancel()
			return nil, ctx.Err()
		})
		variants, receipt, degradation, err := (&Searcher{expansion: config}).expand(ctx, Query{Text: "private query"})
		require.ErrorIs(t, err, context.Canceled)
		assert.Nil(t, variants)
		assert.Nil(t, receipt)
		assert.Equal(t, DegradationNone, degradation)
	})

	t.Run("reranking", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		config := validRerankingConfig()
		config.Provider = rerankingProviderFunc(func(context.Context, RerankingRequest) ([]RerankScore, error) {
			cancel()
			return nil, ctx.Err()
		})
		got, receipt, degradation, err := (&Searcher{reranking: config}).rerank(ctx,
			Query{Text: "private query"}, providerReportFixture())
		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, Report{}, got)
		assert.Nil(t, receipt)
		assert.Equal(t, DegradationNone, degradation)
	})
}

func TestProviderStageRejectsLateSuccessAfterDeadline(t *testing.T) {
	t.Parallel()

	t.Run("expansion", func(t *testing.T) {
		t.Parallel()
		config := validExpansionConfig()
		config.Deadline = time.Millisecond
		config.Provider = queryExpansionProviderFunc(func(ctx context.Context, _ ExpansionRequest) ([]string, error) {
			<-ctx.Done()
			return []string{"late variant"}, nil
		})
		variants, receipt, degradation, err := (&Searcher{expansion: config}).expand(t.Context(), Query{Text: "query"})
		require.NoError(t, err)
		assert.Nil(t, variants)
		assert.Equal(t, &ProviderReceipt{Stage: ProviderStageExpansion, Outcome: ProviderOutcomeTimedOut}, receipt)
		assert.Equal(t, DegradationExpansionDegraded, degradation)
	})

	t.Run("reranking", func(t *testing.T) {
		t.Parallel()
		config := validRerankingConfig()
		config.Deadline = time.Millisecond
		config.Provider = rerankingProviderFunc(func(ctx context.Context, request RerankingRequest) ([]RerankScore, error) {
			<-ctx.Done()
			return exactScores(request.Candidates), nil
		})
		report := providerReportFixture()
		got, receipt, degradation, err := (&Searcher{reranking: config}).rerank(t.Context(), Query{Text: "query"}, report)
		require.NoError(t, err)
		assert.Equal(t, report, got)
		assert.Equal(t, &ProviderReceipt{Stage: ProviderStageReranking,
			Outcome: ProviderOutcomeTimedOut, CandidateCount: 2}, receipt)
		assert.Equal(t, DegradationRerankingDegraded, degradation)
	})
}

func TestProviderStageFailClosedReturnsSanitizedSentinels(t *testing.T) {
	t.Parallel()

	t.Run("expansion", func(t *testing.T) {
		t.Parallel()
		config := validExpansionConfig()
		config.FailurePolicy = ProviderFailureFailClosed
		config.Provider = queryExpansionProviderFunc(func(context.Context, ExpansionRequest) ([]string, error) {
			return nil, errors.New("private provider response")
		})
		variants, receipt, degradation, err := (&Searcher{expansion: config}).expand(t.Context(), Query{Text: "private query"})
		require.ErrorIs(t, err, ErrQueryExpansionFailed)
		assert.Equal(t, ErrQueryExpansionFailed.Error(), err.Error())
		assert.Nil(t, variants)
		assert.Nil(t, receipt)
		assert.Equal(t, DegradationNone, degradation)
	})

	t.Run("reranking", func(t *testing.T) {
		t.Parallel()
		config := validRerankingConfig()
		config.FailurePolicy = ProviderFailureFailClosed
		config.Provider = rerankingProviderFunc(func(context.Context, RerankingRequest) ([]RerankScore, error) {
			return nil, errors.New("private provider response")
		})
		got, receipt, degradation, err := (&Searcher{reranking: config}).rerank(t.Context(),
			Query{Text: "private query"}, providerReportFixture())
		require.ErrorIs(t, err, ErrRerankingFailed)
		assert.Equal(t, ErrRerankingFailed.Error(), err.Error())
		assert.Equal(t, Report{}, got)
		assert.Nil(t, receipt)
		assert.Equal(t, DegradationNone, degradation)
	})
}

func TestProviderStageRejectsOversizedQueryBeforeAuthorizationOrEgress(t *testing.T) {
	t.Parallel()

	oversized := strings.Repeat("q", maxProviderQueryBytes+1)
	expansionAuthorizerCalls, expansionProviderCalls := 0, 0
	expansion := validExpansionConfig()
	expansion.Authorizer = expansionAuthorizerFunc(func(context.Context, ProviderOperation) error {
		expansionAuthorizerCalls++
		return nil
	})
	expansion.Provider = queryExpansionProviderFunc(func(context.Context, ExpansionRequest) ([]string, error) {
		expansionProviderCalls++
		return nil, nil
	})
	_, expansionReceipt, expansionDegradation, err := (&Searcher{expansion: expansion}).expand(t.Context(), Query{Text: oversized})
	require.NoError(t, err)
	assert.Equal(t, 0, expansionAuthorizerCalls)
	assert.Equal(t, 0, expansionProviderCalls)
	assert.Equal(t, &ProviderReceipt{Stage: ProviderStageExpansion, Outcome: ProviderOutcomeMalformed}, expansionReceipt)
	assert.Equal(t, DegradationExpansionDegraded, expansionDegradation)

	rerankingAuthorizerCalls, rerankingProviderCalls := 0, 0
	reranking := validRerankingConfig()
	reranking.Authorizer = rerankingAuthorizerFunc(func(context.Context, ProviderOperation) error {
		rerankingAuthorizerCalls++
		return nil
	})
	reranking.Provider = rerankingProviderFunc(func(context.Context, RerankingRequest) ([]RerankScore, error) {
		rerankingProviderCalls++
		return nil, nil
	})
	report := providerReportFixture()
	got, rerankingReceipt, rerankingDegradation, err := (&Searcher{reranking: reranking}).rerank(t.Context(),
		Query{Text: oversized}, report)
	require.NoError(t, err)
	assert.Equal(t, report, got)
	assert.Equal(t, 0, rerankingAuthorizerCalls)
	assert.Equal(t, 0, rerankingProviderCalls)
	assert.Equal(t, &ProviderReceipt{Stage: ProviderStageReranking,
		Outcome: ProviderOutcomeMalformed, CandidateCount: 2}, rerankingReceipt)
	assert.Equal(t, DegradationRerankingDegraded, rerankingDegradation)
}

func TestProviderStageExpansionUsesExactAuthorizationMetadataAndReceiptCount(t *testing.T) {
	t.Parallel()

	var operation ProviderOperation
	var request ExpansionRequest
	config := validExpansionConfig()
	config.Authorizer = expansionAuthorizerFunc(func(_ context.Context, got ProviderOperation) error {
		operation = got
		return nil
	})
	config.Provider = queryExpansionProviderFunc(func(_ context.Context, got ExpansionRequest) ([]string, error) {
		request = got
		return []string{"zeta", "alpha"}, nil
	})
	scope := store.SearchOptions{TagID: "tag", MIMEType: "text/plain", UnderNodeID: 9,
		ModifiedSince: "2026-01-01", ModifiedBefore: "2026-02-01"}
	variants, receipt, degradation, err := (&Searcher{expansion: config}).expand(t.Context(),
		Query{Text: "query", Scope: scope})
	require.NoError(t, err)
	assert.Equal(t, ProviderOperation{Stage: ProviderStageExpansion, ProfileID: "expansion-profile",
		Scope: scope, InputClass: ProviderInputQueryText, VariantLimit: 2,
		QueryBytes: 5, QueryByteLimit: maxProviderQueryBytes}, operation)
	assert.Equal(t, ExpansionRequest{Query: "query", MaxVariants: 2}, request)
	assert.Equal(t, []string{"alpha", "zeta"}, variants)
	assert.Equal(t, &ProviderReceipt{Stage: ProviderStageExpansion,
		Outcome: ProviderOutcomeApplied, VariantCount: 2}, receipt)
	assert.Equal(t, DegradationNone, degradation)
}

func TestProviderStageRerankingUsesExactBoundedMetadataAndScores(t *testing.T) {
	t.Parallel()

	var operation ProviderOperation
	var request RerankingRequest
	config := validRerankingConfig()
	config.Authorizer = rerankingAuthorizerFunc(func(_ context.Context, got ProviderOperation) error {
		operation = got
		return nil
	})
	config.Provider = rerankingProviderFunc(func(_ context.Context, got RerankingRequest) ([]RerankScore, error) {
		request = got
		return []RerankScore{{Document: got.Candidates[0].Document, Score: 0.25},
			{Document: got.Candidates[1].Document, Score: 0.75}}, nil
	})
	report := providerReportFixture()
	scope := store.SearchOptions{TagID: "tag", MIMEType: "text/plain", UnderNodeID: 9,
		ModifiedSince: "2026-01-01", ModifiedBefore: "2026-02-01"}
	got, receipt, degradation, err := (&Searcher{reranking: config}).rerank(t.Context(),
		Query{Text: "query", Scope: scope}, report)
	require.NoError(t, err)
	assert.Equal(t, ProviderOperation{Stage: ProviderStageReranking, ProfileID: "reranking-profile",
		Scope: scope, InputClass: ProviderInputQueryAndExcerpt, CandidateCount: 2,
		QueryBytes: 5, QueryByteLimit: maxProviderQueryBytes,
		ExcerptBytes: 9, ExcerptBytesPerCandidateLimit: maxRerankingExcerptBytes,
		ExcerptBytesTotalLimit: 2 * maxRerankingExcerptBytes,
		EvidenceCount:          3, EvidencePerCandidateLimit: maxRerankingEvidenceReferences,
		EvidenceTotalLimit: 2 * maxRerankingEvidenceReferences,
		EvidenceBytes:      13, EvidenceBytesPerCandidateLimit: maxRerankingEvidenceBytes,
		EvidenceBytesTotalLimit: 2 * maxRerankingEvidenceBytes}, operation)
	require.Len(t, request.Candidates, 2)
	assert.Equal(t, "first", request.Candidates[0].Excerpt)
	assert.Equal(t, "next", request.Candidates[1].Excerpt)
	assert.Equal(t, "query", request.Query)
	assert.Equal(t, int64(2), got.Results[0].Document.NodeID)
	assert.InDelta(t, 0.75, got.Results[0].Score, 0)
	assert.Equal(t, 1, got.Results[0].Rank)
	assert.Equal(t, int64(1), got.Results[1].Document.NodeID)
	assert.InDelta(t, 0.25, got.Results[1].Score, 0)
	assert.Equal(t, 2, got.Results[1].Rank)
	assert.Equal(t, TraceEvent{Code: TraceRerankedCandidates, Count: 2}, got.Trace[len(got.Trace)-1])
	assert.Equal(t, &ProviderReceipt{Stage: ProviderStageReranking,
		Outcome: ProviderOutcomeApplied, CandidateCount: 2}, receipt)
	assert.Equal(t, DegradationNone, degradation)
}

func TestProviderStageBoundsMultibyteExcerptWithoutSplittingUTF8(t *testing.T) {
	t.Parallel()

	prefix := strings.Repeat("x", maxRerankingExcerptBytes-1)
	excerpt := prefix + "é"
	bounded := boundedRerankingExcerpt(excerpt)

	assert.Len(t, bounded, maxRerankingExcerptBytes-1)
	assert.True(t, utf8.ValidString(bounded))
	assert.Equal(t, prefix, bounded)
}

func TestProviderStageBoundsEvidenceByCountAndBytes(t *testing.T) {
	t.Parallel()

	byCount := make([]EvidenceReference, maxRerankingEvidenceReferences+1)
	for index := range byCount {
		byCount[index] = EvidenceReference{Kind: "k", NodeRevision: int64(index + 1)}
	}
	assert.Len(t, boundedRerankingEvidence(byCount), maxRerankingEvidenceReferences)

	exactlyFull := EvidenceReference{Kind: strings.Repeat("x", maxRerankingEvidenceBytes-3),
		NodeID: 42, NodeRevision: 1}
	byBytes := boundedRerankingEvidence([]EvidenceReference{exactlyFull, {Kind: "k", NodeRevision: 2}})
	require.Len(t, byBytes, 1)
	assert.Equal(t, exactlyFull, byBytes[0])
	assert.Empty(t, boundedRerankingEvidence([]EvidenceReference{{
		Kind: strings.Repeat("x", maxRerankingEvidenceBytes-2), NodeID: 42, NodeRevision: 1,
	}}))
}

func TestProviderStageTotalBoundSaturates(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 0, boundedProviderTotal(maxRerankingExcerptBytes, 0))
	assert.Equal(t, math.MaxInt, boundedProviderTotal(math.MaxInt, 2))
}

func TestProviderStageDegradationPreservesReportAfterProviderMutation(t *testing.T) {
	t.Parallel()

	config := validRerankingConfig()
	config.Provider = rerankingProviderFunc(func(_ context.Context, request RerankingRequest) ([]RerankScore, error) {
		request.Candidates[0].Document.NodeID = 99
		request.Candidates[0].Excerpt = "mutated excerpt"
		request.Candidates[0].Evidence[0].Kind = "mutated evidence"
		request.Candidates[0].Evidence = append(request.Candidates[0].Evidence,
			EvidenceReference{Kind: "injected", NodeRevision: 99})
		return nil, errors.New("private provider response")
	})
	report := providerReportFixture()
	expected := providerReportFixture()

	got, receipt, degradation, err := (&Searcher{reranking: config}).rerank(t.Context(),
		Query{Text: "private query"}, report)

	require.NoError(t, err)
	assert.Equal(t, expected, got)
	assert.Equal(t, &ProviderReceipt{Stage: ProviderStageReranking,
		Outcome: ProviderOutcomeUnavailable, CandidateCount: 2}, receipt)
	assert.Equal(t, DegradationRerankingDegraded, degradation)
}

func validExpansionConfig() ExpansionConfig {
	return ExpansionConfig{Enabled: true, Profile: ExpansionProfile{ID: "expansion-profile", MaxVariants: 2},
		Provider: queryExpansionProviderFunc(func(context.Context, ExpansionRequest) ([]string, error) {
			return []string{"expanded"}, nil
		}), Authorizer: expansionAuthorizerFunc(func(context.Context, ProviderOperation) error { return nil }),
		Deadline: time.Second, FailurePolicy: ProviderFailureDegrade}
}

func validRerankingConfig() RerankingConfig {
	return RerankingConfig{Enabled: true, Profile: RerankingProfile{ID: "reranking-profile", MaxCandidates: 2},
		Provider: rerankingProviderFunc(func(_ context.Context, request RerankingRequest) ([]RerankScore, error) {
			return exactScores(request.Candidates), nil
		}), Authorizer: rerankingAuthorizerFunc(func(context.Context, ProviderOperation) error { return nil }),
		Deadline: time.Second, FailurePolicy: ProviderFailureDegrade}
}

func providerSearcherConfig(expansion ExpansionConfig, reranking RerankingConfig) SearcherConfig {
	return SearcherConfig{Backend: providerBackend{}, Owner: "provider-test", LeaseDuration: time.Minute,
		Expansion: expansion, Reranking: reranking}
}

func providerReportFixture() Report {
	return Report{RequestedMode: ModeLexical, ActualMode: ModeLexical,
		Coverage: Coverage{State: CoverageUnknown},
		Results: []Result{
			{Document: DocumentIdentity{VaultID: "vault", NodeID: 1, ContentVersionID: "version-one"},
				Rank: 1, Score: 10, Path: "/first.txt", Excerpt: "first",
				Evidence: []EvidenceReference{{Kind: "k", NodeID: 11, NodeRevision: 1},
					{Kind: "k", NodeID: 22, NodeRevision: 2}}},
			{Document: DocumentIdentity{VaultID: "vault", NodeID: 2, ContentVersionID: "version-two"},
				Rank: 2, Score: 9, Path: "/next.txt", Excerpt: "next",
				Evidence: []EvidenceReference{{Kind: "k", NodeID: 333, NodeRevision: 3}}},
		}, Trace: []TraceEvent{{Code: TraceLexicalCandidates, Count: 2}}}
}

func exactScores(candidates []RerankingCandidate) []RerankScore {
	scores := make([]RerankScore, len(candidates))
	for index, candidate := range candidates {
		scores[index] = RerankScore{Document: candidate.Document, Score: float64(len(candidates) - index)}
	}
	return scores
}

type providerBackend struct{}

func (providerBackend) VaultID() string { return "vault" }

func (providerBackend) SearchExplainedLexicalCandidates(context.Context, string, int,
	store.SearchOptions,
) ([]store.ExplainedLexicalCandidate, bool, error) {
	return nil, false, nil
}

type queryExpansionProviderFunc func(context.Context, ExpansionRequest) ([]string, error)

func (provider queryExpansionProviderFunc) Expand(ctx context.Context, request ExpansionRequest) ([]string, error) {
	return provider(ctx, request)
}

type rerankingProviderFunc func(context.Context, RerankingRequest) ([]RerankScore, error)

func (provider rerankingProviderFunc) Rerank(ctx context.Context, request RerankingRequest) ([]RerankScore, error) {
	return provider(ctx, request)
}

type expansionAuthorizerFunc func(context.Context, ProviderOperation) error

func (authorizer expansionAuthorizerFunc) AuthorizeExpansion(ctx context.Context, operation ProviderOperation) error {
	return authorizer(ctx, operation)
}

type rerankingAuthorizerFunc func(context.Context, ProviderOperation) error

func (authorizer rerankingAuthorizerFunc) AuthorizeReranking(ctx context.Context, operation ProviderOperation) error {
	return authorizer(ctx, operation)
}
