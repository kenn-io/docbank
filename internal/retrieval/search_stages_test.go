package retrieval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

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
}

func TestSearcherExpansionAuthorizesBeforeProviderAndDegradesSafely(t *testing.T) {
	t.Parallel()

	denied := &stageAuthorizer{err: errors.New("private query: denied")}
	expander := &stageExpander{variants: []string{"replacement"}}
	searcher, _ := stageSearcher(t, func(config *SearcherConfig) {
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
}

func TestSearcherExpansionUsesVariantsWithoutBroadeningScope(t *testing.T) {
	t.Parallel()

	expander := &stageExpander{variants: []string{"zeta", "alpha"}}
	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
		config.Expansion = ExpansionConfig{Enabled: true,
			Profile: ExpansionProfile{ID: "expansion", MaxVariants: 2}, Provider: expander,
			Authorizer: &stageAuthorizer{}, Deadline: time.Second, FailurePolicy: ProviderFailureDegrade}
	})
	backend.hits = []store.ExplainedLexicalCandidate{
		{Node: store.Node{ID: 1, CurrentVersionID: "version-1"}, Path: "/one", EvidenceKind: "rendition_segment",
			BuildID: "build-1", SegmentID: "segment-1", Locator: document.EvidenceLocatorV1{
				Kind: document.EvidenceLocatorSegment, IndexOrigin: document.EvidenceIndexOriginZero, Start: 0, End: 1000}},
		{Node: store.Node{ID: 2, CurrentVersionID: "version-2"}, Path: "/two", EvidenceKind: "node_name"},
	}
	scope := store.SearchOptions{TagID: "tag", MIMEType: "text/plain", UnderNodeID: 7,
		ModifiedSince: "2026-01-01T00:00:00Z", ModifiedBefore: "2026-12-31T00:00:00Z"}

	report, err := searcher.Search(t.Context(), Query{Text: "original", Mode: ModeLexical, Limit: 2, Scope: scope})
	require.NoError(t, err)
	assert.Equal(t, []string{"original", "alpha", "zeta"}, backend.queries)
	assert.Equal(t, []store.SearchOptions{scope, scope, scope}, backend.scopes)
	assert.LessOrEqual(t, len(report.Results), 2)
	for _, result := range report.Results {
		assert.Len(t, result.Evidence, 1)
	}
	assert.Equal(t, []ProviderReceipt{{Stage: ProviderStageExpansion,
		Outcome: ProviderOutcomeApplied, VariantCount: 2}}, report.Receipts)
}

func TestSearcherRerankingTimesOutAndReportsNamedDegradation(t *testing.T) {
	t.Parallel()

	reranker := &stageReranker{wait: true}
	searcher, _ := stageSearcher(t, func(config *SearcherConfig) {
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
	assert.NotContains(t, err.Error(), "private query")
	assert.NotContains(t, err.Error(), "provider body")
	assert.NotContains(t, err.Error(), "credential")
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

func TestSearcherRerankingReordersOnlyAuthorizedCandidates(t *testing.T) {
	t.Parallel()

	reranker := &stageReranker{scores: []RerankScore{
		{Document: DocumentIdentity{VaultID: "vault", NodeID: 1, ContentVersionID: "version-1"}, Score: 1},
		{Document: DocumentIdentity{VaultID: "vault", NodeID: 2, ContentVersionID: "version-2"}, Score: 2},
	}}
	searcher, _ := stageSearcher(t, func(config *SearcherConfig) {
		config.Reranking = RerankingConfig{Enabled: true,
			Profile: RerankingProfile{ID: "reranking", MaxCandidates: 2}, Provider: reranker,
			Authorizer: &stageAuthorizer{}, Deadline: time.Second, FailurePolicy: ProviderFailureFailClosed}
	})

	report, err := searcher.Search(t.Context(), Query{Text: "original", Mode: ModeLexical, Limit: 2})
	require.NoError(t, err)
	assert.Equal(t, int64(2), report.Results[0].Document.NodeID)
	assert.Equal(t, int64(1), report.Results[1].Document.NodeID)
	assert.Equal(t, []RerankingCandidate{
		{Document: DocumentIdentity{VaultID: "vault", NodeID: 1, ContentVersionID: "version-1"}, Excerpt: "one", Evidence: []EvidenceReference{{Kind: "node_name", VaultID: "vault", NodeID: 1, ContentVersionID: "version-1"}}},
		{Document: DocumentIdentity{VaultID: "vault", NodeID: 2, ContentVersionID: "version-2"}, Excerpt: "two", Evidence: []EvidenceReference{{Kind: "node_name", VaultID: "vault", NodeID: 2, ContentVersionID: "version-2"}}},
	}, reranker.candidates)
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
		{Node: store.Node{ID: 1, CurrentVersionID: "version-1", Name: "one"}, Path: "/one",
			EvidenceKind: "rendition_segment", BuildID: "build-1", SegmentID: "segment-1",
			Excerpt: strings.Repeat("x", maxRerankingExcerptBytes+1)},
		{Node: store.Node{ID: 2, CurrentVersionID: "version-2", Name: "two"}, Path: "/two",
			EvidenceKind: "rendition_segment", BuildID: "build-2", SegmentID: "segment-2", Excerpt: "out-of-scope"},
	}
	scope := store.SearchOptions{TagID: "tag", UnderNodeID: 7}

	_, err := searcher.Search(t.Context(), Query{Text: "private query", Mode: ModeLexical, Limit: 1, Scope: scope})
	require.NoError(t, err)
	require.Len(t, reranker.candidates, 1)
	assert.Equal(t, DocumentIdentity{VaultID: "vault", NodeID: 1, ContentVersionID: "version-1"}, reranker.candidates[0].Document)
	assert.Len(t, reranker.candidates[0].Excerpt, maxRerankingExcerptBytes)
	assert.Equal(t, []EvidenceReference{{Kind: "rendition_segment", VaultID: "vault", NodeID: 1,
		ContentVersionID: "version-1", BuildID: "build-1", SegmentID: "segment-1"}}, reranker.candidates[0].Evidence)
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

func TestRerankingEvidenceKeepsIndependentTimedLanes(t *testing.T) {
	t.Parallel()

	lexicalSpan := &MediaTimeSpan{StartMS: 0, EndMS: 1000}
	semanticSpan := &MediaTimeSpan{StartMS: 7000, EndMS: 9500}
	references := []EvidenceReference{
		{Kind: "rendition_segment", BuildID: "build", SegmentID: "segment", TimeSpan: lexicalSpan},
		{Kind: "embedding", BuildID: "build", InputGenerationID: "generation", InputID: "input",
			InputKind: document.EmbeddingInputRenditionChunk, TimeSpan: semanticSpan},
	}

	bounded := boundedRerankingEvidence(references)
	require.Equal(t, []EvidenceReference{
		{Kind: "rendition_segment", BuildID: "build", SegmentID: "segment",
			TimeSpan: &MediaTimeSpan{StartMS: 0, EndMS: 1000}},
		{Kind: "embedding", BuildID: "build", InputGenerationID: "generation", InputID: "input",
			InputKind: document.EmbeddingInputRenditionChunk,
			TimeSpan:  &MediaTimeSpan{StartMS: 7000, EndMS: 9500}},
	}, bounded)

	bounded[0].TimeSpan.EndMS = 2000
	require.Equal(t, int64(1000), references[0].TimeSpan.EndMS)
}

func TestSearcherRerankingAuthorizationBindsPerCandidateAndTotalPayloadLimits(t *testing.T) {
	t.Parallel()

	authorizer := &stageAuthorizer{}
	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
		config.Reranking = RerankingConfig{Enabled: true,
			Profile: RerankingProfile{ID: "reranking", MaxCandidates: 2}, Provider: &stageReranker{},
			Authorizer: authorizer, Deadline: time.Second, FailurePolicy: ProviderFailureFailClosed}
	})
	backend.hits = []store.ExplainedLexicalCandidate{
		{Node: store.Node{ID: 1, CurrentVersionID: "version-1", Name: "one"}, Path: "/one",
			EvidenceKind: "rendition_segment", BuildID: "build-1", SegmentID: "segment-1",
			Excerpt: strings.Repeat("a", maxRerankingExcerptBytes+1)},
		{Node: store.Node{ID: 2, CurrentVersionID: "version-2", Name: "two"}, Path: "/two",
			EvidenceKind: "rendition_segment", BuildID: "build-2", SegmentID: "segment-2",
			Excerpt: strings.Repeat("b", maxRerankingExcerptBytes+1)},
	}

	_, err := searcher.Search(t.Context(), Query{Text: "private query", Mode: ModeLexical, Limit: 2})
	require.NoError(t, err)
	require.Len(t, authorizer.operations, 1)
	operation := authorizer.operations[0]
	assert.Equal(t, 2*maxRerankingExcerptBytes, operation.ExcerptBytes)
	assert.Equal(t, maxRerankingExcerptBytes, operation.ExcerptBytesPerCandidateLimit)
	assert.Equal(t, 2*maxRerankingExcerptBytes, operation.ExcerptBytesTotalLimit)
	assert.Equal(t, 2, operation.EvidenceCount)
	assert.Equal(t, maxRerankingEvidenceReferences, operation.EvidencePerCandidateLimit)
	assert.Equal(t, 2*maxRerankingEvidenceReferences, operation.EvidenceTotalLimit)
	assert.Positive(t, operation.EvidenceBytes)
	assert.Equal(t, maxRerankingEvidenceBytes, operation.EvidenceBytesPerCandidateLimit)
	assert.Equal(t, 2*maxRerankingEvidenceBytes, operation.EvidenceBytesTotalLimit)
}

func TestSearcherRerankingAuthorizationPrecedesProviderEgress(t *testing.T) {
	t.Parallel()

	authorizer := &stageAuthorizer{err: errors.New("denied")}
	reranker := &stageReranker{}
	searcher, _ := stageSearcher(t, func(config *SearcherConfig) {
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
	assert.Equal(t, DegradationRerankingDegraded, report.Degradations[0])
}

func TestSearcherRerankingUsesConfiguredExcerptBoundAndSkipsEmptyResults(t *testing.T) {
	t.Run("configured excerpt bound", func(t *testing.T) {
		authorizer := &stageAuthorizer{}
		reranker := &stageReranker{}
		searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
			config.Reranking = RerankingConfig{Enabled: true,
				Profile:  RerankingProfile{ID: "reranking", MaxCandidates: 1, MaxExcerptBytes: 7},
				Provider: reranker, Authorizer: authorizer, Deadline: time.Second,
				FailurePolicy: ProviderFailureDegrade}
		})
		backend.hits = []store.ExplainedLexicalCandidate{{
			Node: store.Node{ID: 1, CurrentVersionID: "version-1"}, Path: "/one",
			EvidenceKind: "node_name", Excerpt: "abcdefghi",
		}}

		report, err := searcher.Search(t.Context(), Query{Text: "query", Mode: ModeLexical, Limit: 1})
		require.NoError(t, err)
		require.Len(t, report.Receipts, 1)
		assert.Equal(t, 1, report.Receipts[0].CandidateCount)
		assert.Equal(t, "abcdefg", reranker.candidates[0].Excerpt)
		assert.Equal(t, 7, authorizer.operations[0].ExcerptBytes)
	})

	t.Run("empty results are skipped", func(t *testing.T) {
		reranker := &stageReranker{}
		searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
			config.Reranking = RerankingConfig{Enabled: true,
				Profile:  RerankingProfile{ID: "reranking", MaxCandidates: 1},
				Provider: reranker, Authorizer: &stageAuthorizer{}, Deadline: time.Second,
				FailurePolicy: ProviderFailureDegrade}
		})
		backend.hits = []store.ExplainedLexicalCandidate{}

		report, err := searcher.Search(t.Context(), Query{Text: "query", Mode: ModeLexical, Limit: 1})
		require.NoError(t, err)
		assert.Zero(t, reranker.calls)
		require.Len(t, report.Receipts, 1)
		assert.Equal(t, ProviderOutcomeSkipped, report.Receipts[0].Outcome)
		assert.Zero(t, report.Receipts[0].CandidateCount)
	})
	t.Logf("configured=7 skipped=%s invalid=-1,4097 legacy=%d", ProviderOutcomeSkipped, maxRerankingExcerptBytes)
}

func TestSearcherRejectsInvalidConfiguredExcerptBounds(t *testing.T) {
	for _, bound := range []int{-1, maxRerankingExcerptBytes + 1} {
		_, err := NewSearcher(SearcherConfig{Backend: &stageBackend{}, Owner: "retrieval-test",
			LeaseDuration: time.Minute, Reranking: RerankingConfig{Enabled: true,
				Profile:  RerankingProfile{ID: "reranking", MaxCandidates: 1, MaxExcerptBytes: bound},
				Provider: &stageReranker{}, Authorizer: &stageAuthorizer{}, Deadline: time.Second,
				FailurePolicy: ProviderFailureDegrade}})
		require.Error(t, err)
	}
}

func TestSearcherRerankingNeverSendsEmptyExcerpt(t *testing.T) {
	searcher, _, _, descriptor := retrievalSearcherFixture(t, true, 1)
	reranker := &stageReranker{}
	searcher.reranking = RerankingConfig{Enabled: true,
		Profile: RerankingProfile{ID: "reranking", MaxCandidates: 1}, Provider: reranker,
		Authorizer: &stageAuthorizer{}, Deadline: time.Second, FailurePolicy: ProviderFailureDegrade}

	report, err := searcher.Search(t.Context(), Query{Text: "query", Mode: ModeSemantic, Limit: 1,
		ProcessingProfileFingerprint: "profile", BindingID: "required", Authorization: retrievalAuthorization(descriptor)})
	require.NoError(t, err)
	assert.Zero(t, reranker.calls)
	assert.Equal(t, []Degradation{DegradationRerankingDegraded}, report.Degradations)
	assert.Equal(t, []ProviderReceipt{{Stage: ProviderStageReranking,
		Outcome: ProviderOutcomeMalformed, CandidateCount: 1}}, report.Receipts)
}

func TestSearcherRerankingRejectsEmptyExcerptAfterUTF8Bound(t *testing.T) {
	reranker := &stageReranker{}
	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
		config.Reranking = RerankingConfig{Enabled: true,
			Profile: RerankingProfile{ID: "reranking", MaxCandidates: 1, MaxExcerptBytes: 1}, Provider: reranker,
			Authorizer: &stageAuthorizer{}, Deadline: time.Second, FailurePolicy: ProviderFailureDegrade}
	})
	backend.hits = []store.ExplainedLexicalCandidate{{
		Node: store.Node{ID: 1, CurrentVersionID: "version-1"}, Path: "/one",
		EvidenceKind: "node_name", Excerpt: "报告"}}

	report, err := searcher.Search(t.Context(), Query{Text: "query", Mode: ModeLexical, Limit: 1})
	require.NoError(t, err)
	assert.Zero(t, reranker.calls)
	assert.Equal(t, []Degradation{DegradationRerankingDegraded}, report.Degradations)
	assert.Equal(t, []ProviderReceipt{{Stage: ProviderStageReranking,
		Outcome: ProviderOutcomeMalformed, CandidateCount: 1}}, report.Receipts)
}

func TestSearcherRerankingPayloadKeepsPublicExcerpt(t *testing.T) {
	for _, test := range []struct {
		name, kind, lexical, semantic, want string
		mode                                Mode
	}{
		{"semantic", "", "", "current semantic source", "current semantic source", ModeSemantic},
		{"hybrid name", "node_name", "semantic.pdf", "current semantic source", "current semantic source", ModeHybrid},
		{"hybrid content", "rendition_segment", "matching lexical text", "current semantic source", "matching lexical text", ModeHybrid},
		{"hybrid missing content", "node_name", "semantic.pdf", "", "semantic.pdf", ModeHybrid},
		{"hybrid blank content", "node_name", "semantic.pdf", " \n\t", "semantic.pdf", ModeHybrid},
	} {
		t.Run(test.name, func(t *testing.T) {
			searcher, backend, _, descriptor := retrievalSearcherFixture(t, true, 1)
			backend.semantic[0].Excerpt = test.semantic
			if test.kind != "" {
				backend.lexical = []store.ExplainedLexicalCandidate{{
					Node: store.Node{ID: 8, CurrentVersionID: "version-semantic", Name: "semantic.pdf"},
					Path: "/semantic.pdf", EvidenceKind: test.kind, Excerpt: test.lexical,
				}}
			}
			reranker := &stageReranker{}
			searcher.reranking = RerankingConfig{Enabled: true,
				Profile: RerankingProfile{ID: "reranking", MaxCandidates: 1}, Provider: reranker,
				Authorizer: &stageAuthorizer{}, Deadline: time.Second, FailurePolicy: ProviderFailureDegrade}
			ranked, err := searcher.Search(t.Context(), Query{Text: "query", Mode: test.mode, Limit: 1,
				ProcessingProfileFingerprint: "profile", BindingID: "required", Authorization: retrievalAuthorization(descriptor)})
			require.NoError(t, err)
			require.Len(t, reranker.candidates, 1)
			assert.Equal(t, test.want, reranker.candidates[0].Excerpt)
			require.Len(t, ranked.Results, 1)
			assert.Equal(t, test.lexical, ranked.Results[0].Excerpt)
		})
	}
}

func TestSearcherSemanticRerankingFailsClosedWithoutExcerpt(t *testing.T) {
	searcher, _, _, descriptor := retrievalSearcherFixture(t, true, 1)
	reranker := &stageReranker{}
	searcher.reranking = RerankingConfig{Enabled: true,
		Profile: RerankingProfile{ID: "reranking", MaxCandidates: 1}, Provider: reranker,
		Authorizer: &stageAuthorizer{}, Deadline: time.Second, FailurePolicy: ProviderFailureFailClosed}

	_, err := searcher.Search(t.Context(), Query{Text: "query", Mode: ModeSemantic, Limit: 1,
		ProcessingProfileFingerprint: "profile", BindingID: "required", Authorization: retrievalAuthorization(descriptor)})
	require.ErrorIs(t, err, ErrRerankingFailed)
	assert.Zero(t, reranker.calls)
}

func TestSearcherRerankOnlyRevalidatesRevokedEvidenceBeforeProviderEgress(t *testing.T) {
	t.Parallel()

	reranker := &stageReranker{}
	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
		config.Reranking = RerankingConfig{Enabled: true,
			Profile: RerankingProfile{ID: "reranking", MaxCandidates: 2}, Provider: reranker,
			Authorizer: &stageAuthorizer{}, Deadline: time.Second, FailurePolicy: ProviderFailureFailClosed}
	})
	backend.hits = []store.ExplainedLexicalCandidate{
		{Node: store.Node{ID: 1, CurrentVersionID: "version-1", Name: "one"}, Path: "/one",
			EvidenceKind: "rendition_segment", BuildID: "build-1", SegmentID: "segment-1", Excerpt: "still visible"},
		{Node: store.Node{ID: 2, CurrentVersionID: "version-2", Name: "two"}, Path: "/two",
			EvidenceKind: "rendition_segment", BuildID: "build-2", SegmentID: "segment-2", Excerpt: "revoked excerpt"},
	}
	backend.revalidated = []store.RevalidatedSearchCandidate{{NodeID: 1, ContentVersionID: "version-1"}}

	report, err := searcher.Search(t.Context(), Query{Text: "original", Mode: ModeLexical, Limit: 2})
	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	require.Len(t, reranker.candidates, 1)
	assert.Equal(t, int64(1), reranker.candidates[0].Document.NodeID)
	assert.NotContains(t, fmt.Sprintf("%#v", reranker.candidates), "revoked excerpt")
	assert.Equal(t, 1, backend.revalidationCalls)
}

func TestSearcherRerankOnlyFailsClosedWithoutCandidateRevalidation(t *testing.T) {
	t.Parallel()

	backend := &nonRevalidatingStageBackend{inner: &stageBackend{}}
	reranker := &stageReranker{}
	searcher, err := NewSearcher(SearcherConfig{Backend: backend, Owner: "retrieval-test",
		LeaseDuration: time.Minute, Clock: time.Now, Reranking: RerankingConfig{Enabled: true,
			Profile: RerankingProfile{ID: "reranking", MaxCandidates: 2}, Provider: reranker,
			Authorizer: &stageAuthorizer{}, Deadline: time.Second, FailurePolicy: ProviderFailureFailClosed}})
	require.NoError(t, err)

	_, err = searcher.Search(t.Context(), Query{Text: "original", Mode: ModeLexical, Limit: 2})
	require.ErrorIs(t, err, ErrRerankingFailed)
	assert.Zero(t, reranker.calls)
}

func TestMergeVariantReportsConservativelyAggregatesCoverage(t *testing.T) {
	t.Parallel()

	first := Report{RequestedMode: ModeSemantic, ActualMode: ModeSemantic,
		Coverage: Coverage{BindingRequired: true, ScopedDocuments: 1, CompleteDocuments: 1, State: CoverageComplete}}
	second := Report{RequestedMode: ModeSemantic, ActualMode: ModeSemantic,
		Coverage: Coverage{BindingRequired: true, ScopedDocuments: 2, CompleteDocuments: 1, State: CoverageIncomplete}}

	merged, err := mergeVariantReports([]Report{first, second}, 1)
	require.NoError(t, err)
	assert.Equal(t, Coverage{BindingRequired: true, ScopedDocuments: 2, CompleteDocuments: 1,
		State: CoverageIncomplete}, merged.Coverage)
}

func TestMergeVariantReportsRejectsIncompatibleDegradation(t *testing.T) {
	t.Parallel()

	_, err := mergeVariantReports([]Report{
		{RequestedMode: ModeLexical, ActualMode: ModeLexical, Degradations: nil},
		{RequestedMode: ModeLexical, ActualMode: ModeLexical, Degradations: []Degradation{DegradationExpansionDegraded}},
	}, 1)
	require.Error(t, err)
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
	backend.revalidated = []store.RevalidatedSearchCandidate{{NodeID: 1, ContentVersionID: "version-1"}}

	report, err := searcher.Search(t.Context(), Query{Text: "original", Mode: ModeLexical, Limit: 2,
		Scope: store.SearchOptions{TagID: "tag"}})
	require.NoError(t, err)
	assert.Len(t, report.Results, 1)
	assert.Len(t, reranker.candidates, 1)
	assert.Equal(t, int64(1), reranker.candidates[0].Document.NodeID)
	assert.Equal(t, 1, backend.revalidationCalls)
}

func TestExpandedSemanticSearchReportsCoverageChangesAtFinalFence(t *testing.T) {
	t.Parallel()

	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {})
	backend.revalidated = []store.RevalidatedSearchCandidate{{NodeID: 1, ContentVersionID: "version-1"}}
	backend.revalidationCoverage = &store.SearchCoverageSnapshot{ScopedDocuments: 2, CompleteDocuments: 1}
	report := Report{RequestedMode: ModeSemantic, ActualMode: ModeSemantic,
		Coverage: Coverage{BindingRequired: true, ScopedDocuments: 1, CompleteDocuments: 1,
			State: CoverageComplete},
		Results: []Result{{Document: DocumentIdentity{VaultID: "vault", NodeID: 1,
			ContentVersionID: "version-1"}, Evidence: []EvidenceReference{{Kind: "node_name"}}}}}

	revalidated, err := searcher.revalidateReport(t.Context(), Query{
		ProcessingProfileFingerprint: "profile", BindingID: "required"}, report)

	require.NoError(t, err)
	assert.Equal(t, Coverage{BindingRequired: true, ScopedDocuments: 2, CompleteDocuments: 1,
		State: CoverageIncomplete}, revalidated.Coverage)
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
	assert.NotContains(t, err.Error(), "original secret")
	assert.NotContains(t, err.Error(), "expanded secret")
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
	searcher, _ := stageSearcher(t, func(config *SearcherConfig) {
		config.Reranking = RerankingConfig{Enabled: true,
			Profile: RerankingProfile{ID: "reranking", MaxCandidates: MaxCandidateLimit}, Provider: reranker,
			Authorizer: &stageAuthorizer{}, Deadline: time.Second, FailurePolicy: ProviderFailureFailClosed}
	})

	report, err := searcher.Search(t.Context(), Query{Text: "original", Mode: ModeLexical, Limit: 1})
	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	assert.Equal(t, []RerankingCandidate{{Document: DocumentIdentity{VaultID: "vault", NodeID: 1, ContentVersionID: "version-1"},
		Excerpt: "one", Evidence: []EvidenceReference{{Kind: "node_name", VaultID: "vault", NodeID: 1, ContentVersionID: "version-1"}}}}, reranker.candidates)
}

func TestSearcherBoundsSemanticCandidatesToTheRequestedLimit(t *testing.T) {
	t.Parallel()

	searcher, backend, _, descriptor := retrievalSearcherFixture(t, true, 1)
	backend.semantic = append(backend.semantic, store.SemanticSearchCandidate{VaultID: "vault", NodeID: 9,
		ContentVersionID: "version-extra", Path: "/extra", VectorSpaceID: backend.authority.VectorSpace.ID,
		EmbeddingSetID: "set-extra", InputGenerationID: "generation-extra", InputID: "input-extra",
		InputKind: document.EmbeddingInputOriginalFile, Score: 0.5})

	report, err := searcher.Search(t.Context(), Query{Text: "original", Mode: ModeSemantic, Limit: 1,
		ProcessingProfileFingerprint: "profile", BindingID: "required", Authorization: retrievalAuthorization(descriptor)})
	require.NoError(t, err)
	assert.Len(t, report.Results, 1)
	assert.True(t, report.Truncated)
}

type stageBackend struct {
	queries              []string
	scopes               []store.SearchOptions
	hits                 []store.ExplainedLexicalCandidate
	revalidated          []store.RevalidatedSearchCandidate
	revalidationCoverage *store.SearchCoverageSnapshot
	revalidationCalls    int
	errForQuery          map[string]error
	cancelForQuery       string
	cancel               context.CancelFunc
}

type nonRevalidatingStageBackend struct{ inner *stageBackend }

func (backend *nonRevalidatingStageBackend) NormalizeSearchOptions(ctx context.Context, options store.SearchOptions) (store.SearchOptions, error) {
	return backend.inner.NormalizeSearchOptions(ctx, options)
}

func (backend *nonRevalidatingStageBackend) VaultID() string { return backend.inner.VaultID() }

func (backend *nonRevalidatingStageBackend) SearchExplainedLexicalCandidates(ctx context.Context,
	query string, limit int, scope store.SearchOptions,
) ([]store.ExplainedLexicalCandidate, bool, error) {
	return backend.inner.SearchExplainedLexicalCandidates(ctx, query, limit, scope)
}

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
		hits = []store.ExplainedLexicalCandidate{
			{Node: store.Node{ID: 1, CurrentVersionID: "version-1", Name: "one"}, Path: "/one", EvidenceKind: "node_name", Excerpt: "one"},
			{Node: store.Node{ID: 2, CurrentVersionID: "version-2", Name: "two"}, Path: "/two", EvidenceKind: "node_name", Excerpt: "two"},
		}
	}
	return hits, false, nil
}

func (backend *stageBackend) RevalidateSearchCandidates(_ context.Context,
	_ []store.SearchCandidateIdentity, _ store.SearchOptions, _, _ string,
) (store.SearchCandidateRevalidation, error) {
	backend.revalidationCalls++
	if backend.revalidated != nil {
		return store.SearchCandidateRevalidation{Candidates: backend.revalidated,
			Coverage: backend.revalidationCoverage}, nil
	}
	return store.SearchCandidateRevalidation{Candidates: []store.RevalidatedSearchCandidate{
		{NodeID: 1, ContentVersionID: "version-1"}, {NodeID: 2, ContentVersionID: "version-2"}},
		Coverage: backend.revalidationCoverage}, nil
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
	operations []ProviderOperation
}

func (authorizer *stageAuthorizer) AuthorizeExpansion(_ context.Context, operation ProviderOperation) (ProviderEgressLease, error) {
	authorizer.operations = append(authorizer.operations, operation)
	if authorizer.err != nil {
		return nil, authorizer.err
	}
	return authorizer, nil
}

func (authorizer *stageAuthorizer) AuthorizeReranking(_ context.Context, operation ProviderOperation) (ProviderEgressLease, error) {
	authorizer.operations = append(authorizer.operations, operation)
	if authorizer.err != nil {
		return nil, authorizer.err
	}
	return authorizer, nil
}

type stageExpander struct {
	variants        []string
	err             error
	waitThenSucceed bool
	calls           int
}

func (provider *stageExpander) Expand(ctx context.Context, _ ExpansionRequest) ([]string, error) {
	provider.calls++
	if provider.waitThenSucceed {
		<-ctx.Done()
		return provider.variants, nil
	}
	return provider.variants, provider.err
}

type stageReranker struct {
	scores     []RerankScore
	candidates []RerankingCandidate
	wait       bool
	calls      int
}

func (provider *stageReranker) Rerank(ctx context.Context, request RerankingRequest) ([]RerankScore, error) {
	provider.calls++
	provider.candidates = append([]RerankingCandidate(nil), request.Candidates...)
	if provider.wait {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if provider.scores != nil {
		return provider.scores, nil
	}
	scores := make([]RerankScore, len(request.Candidates))
	for index, candidate := range request.Candidates {
		scores[index] = RerankScore{Document: candidate.Document, Score: float64(len(scores) - index)}
	}
	return scores, nil
}

func (backend *stageBackend) NormalizeSearchOptions(_ context.Context, options store.SearchOptions) (store.SearchOptions, error) {
	return options, nil
}

func (authorizer *stageAuthorizer) Close() {}
