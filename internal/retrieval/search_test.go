package retrieval

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorindex"
)

func TestSearcherLexicalModePreservesStoreOrderAndStableEvidence(t *testing.T) {
	t.Parallel()
	backend := &retrievalBackendStub{vaultID: "vault", lexical: []store.ExplainedLexicalCandidate{
		{Node: store.Node{ID: 4, CurrentVersionID: "version-name", Name: "alpha.pdf"},
			Path: "/alpha.pdf", Match: store.SearchMatchName, EvidenceKind: "node_name", Excerpt: "alpha.pdf"},
		{Node: store.Node{ID: 2, CurrentVersionID: "version-content", Name: "notes.pdf"},
			Path: "/notes.pdf", Match: store.SearchMatchContent, EvidenceKind: "rendition_segment",
			BuildID: "build", SegmentID: "segment", Excerpt: "alpha excerpt"},
	}}
	searcher, err := NewSearcher(SearcherConfig{Backend: backend, Owner: "retrieval-test",
		LeaseDuration: time.Minute, Clock: time.Now})
	require.NoError(t, err)
	report, err := searcher.Search(t.Context(), Query{Text: "alpha", Mode: ModeLexical, Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, ModeLexical, report.ActualMode)
	require.Len(t, report.Results, 2)
	assert.Equal(t, int64(4), report.Results[0].Document.NodeID)
	assert.Equal(t, "alpha.pdf", report.Results[0].Excerpt)
	assert.Equal(t, "version-content", report.Results[1].Evidence[0].ContentVersionID)
	assert.Equal(t, "build", report.Results[1].Evidence[0].BuildID)
	assert.Equal(t, "segment", report.Results[1].Evidence[0].SegmentID)
	assert.Equal(t, "alpha excerpt", report.Results[1].Excerpt)
}

// This test fails if a query encoder can be configured without the separate
// consent boundary that must run immediately before provider egress.
func TestNewSearcherRequiresQueryEmbeddingAuthorizerWithEncoder(t *testing.T) {
	t.Parallel()
	descriptor, _ := retrievalVectorFixture(t)
	_, err := NewSearcher(SearcherConfig{
		Backend:  &retrievalBackendStub{vaultID: "vault"},
		Encoders: &retrievalResolver{provider: &retrievalProvider{descriptor: descriptor}},
		Owner:    "retrieval-test", LeaseDuration: time.Minute,
	})
	require.ErrorContains(t, err, "query embedding authorizer")
}

// This test fails if consent denial or a current revocation can reach the
// encoder, or if the consent check is not bound to the exact private operation.
func TestSearcherAuthorizesExactQueryEmbeddingImmediatelyBeforeProvider(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "denied", err: errors.New("synthetic query embedding denied")},
		{name: "revoked", err: store.ErrProcessingConsentRevoked},
	} {
		t.Run(test.name, func(t *testing.T) {
			descriptor, generation := retrievalVectorFixture(t)
			disclosure := strings.Repeat("d", 64)
			profile := strings.Repeat("a", 64)
			scope := store.SearchOptions{TagID: "tag-synthetic", MIMEType: "text/plain", UnderNodeID: 17}
			backend := &retrievalBackendStub{vaultID: "vault", authority: store.SemanticSearchAuthority{
				VectorSpace: store.EmbeddingVectorSpaceRecord{ID: generation.Metadata().VectorSpaceID, Descriptor: descriptor},
				Lease: store.VectorIndexReaderLease{ID: "lease", FencingToken: 1,
					Generation: store.VectorIndexGenerationRecord{ID: strings.Repeat("5", 64),
						VectorSpaceID: generation.Metadata().VectorSpaceID, Bytes: generation.Bytes(),
						SourceManifestChecksum: strings.Repeat("c", 64),
						IndexManifestChecksum:  generation.Metadata().Manifest.Checksum,
						RowCount:               generation.Metadata().RowCount}},
				DisclosureFingerprint: disclosure, ScopedDocuments: 1, CompleteDocuments: 1,
				ANNRows: retrievalANNRows(t, generation),
			}}
			provider := &retrievalProvider{descriptor: descriptor, vector: []float32{1, 0}}
			authorizer := &retrievalQueryAuthorizer{err: test.err}
			searcher, err := NewSearcher(SearcherConfig{Backend: backend,
				Encoders: &retrievalResolver{provider: provider}, QueryEmbeddingAuthorizer: authorizer,
				Owner: "retrieval-test", LeaseDuration: time.Minute, Clock: time.Now})
			require.NoError(t, err)

			_, err = searcher.Search(t.Context(), Query{Text: "private semantic query", Mode: ModeSemantic,
				Limit: 1, Scope: scope, ProcessingProfileFingerprint: profile, BindingID: "required",
				Authorization: retrievalAuthorization(descriptor)})
			require.ErrorIs(t, err, test.err)
			assert.Zero(t, provider.calls, "authorization failure must execute no provider code")
			require.Equal(t, []QueryEmbeddingOperation{{
				ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
				PolicyFingerprint: descriptor.PolicyFingerprint, ProfileFingerprint: profile,
				DisclosureFingerprint: disclosure, Scope: scope, InputClass: ProviderInputQueryText,
			}}, authorizer.operations)
		})
	}
}

func TestSearcherSemanticUsesExactDescriptorAndExhaustsLeasedGeneration(t *testing.T) {
	t.Parallel()
	descriptor, generation := retrievalVectorFixture(t)
	backend := &retrievalBackendStub{vaultID: "vault", authority: store.SemanticSearchAuthority{
		VectorSpace: store.EmbeddingVectorSpaceRecord{ID: generation.Metadata().VectorSpaceID, Descriptor: descriptor},
		Lease: store.VectorIndexReaderLease{ID: "lease", FencingToken: 4,
			Generation: store.VectorIndexGenerationRecord{ID: strings.Repeat("1", 64),
				VectorSpaceID: generation.Metadata().VectorSpaceID, Bytes: generation.Bytes(),
				SourceManifestChecksum: strings.Repeat("c", 64),
				IndexManifestChecksum:  generation.Metadata().Manifest.Checksum,
				RowCount:               generation.Metadata().RowCount}},
		InputKind: document.EmbeddingInputOriginalFile, BindingRequired: true,
		ScopedDocuments: 1, CompleteDocuments: 1, ANNRows: retrievalANNRows(t, generation),
	}, semantic: []store.SemanticSearchCandidate{{VaultID: "vault", NodeID: 8,
		ContentVersionID: "version-semantic", Path: "/semantic.pdf",
		VectorSpaceID: generation.Metadata().VectorSpaceID, EmbeddingSetID: "set",
		InputGenerationID: "generation", InputID: "input",
		InputKind: document.EmbeddingInputOriginalFile, Score: 0.9}}}
	provider := &retrievalProvider{descriptor: descriptor, vector: []float32{1, 0}}
	resolver := &retrievalResolver{provider: provider}
	clockCalls := 0
	start := time.Date(2026, 8, 26, 13, 0, 0, 0, time.UTC)
	searcher, err := NewSearcher(SearcherConfig{Backend: backend, Encoders: resolver,
		QueryEmbeddingAuthorizer: &retrievalQueryAuthorizer{},
		Owner:                    "retrieval-test", LeaseDuration: time.Minute, Clock: func() time.Time {
			clockCalls++
			return start.Add(time.Duration(clockCalls) * time.Second)
		}})
	require.NoError(t, err)
	report, err := searcher.Search(t.Context(), Query{Text: "semantic query", Mode: ModeSemantic,
		Limit: 1, ProcessingProfileFingerprint: strings.Repeat("a", 64), BindingID: "required",
		Authorization: retrievalAuthorization(descriptor)})
	require.NoError(t, err)
	assert.Equal(t, descriptor, resolver.requested)
	assert.Equal(t, descriptor.ModelInput.EncodeQuery("semantic query"), provider.rendered)
	assert.Equal(t, backend.authority.Lease.Generation.SourceManifestChecksum, backend.sourceManifest)
	assert.Equal(t, generation.Metadata().RowCount, backend.neighborCount)
	assert.True(t, backend.releasedAt.After(backend.acquiredAt))
	assert.Equal(t, ModeSemantic, report.ActualMode)
	require.Len(t, report.Results, 1)
	assert.Empty(t, report.Results[0].Excerpt)
	assert.Equal(t, document.EmbeddingInputOriginalFile, report.Results[0].Evidence[0].InputKind)
}

func TestSearcherRejectsChangedQueryModelInputBeforeProviderOrIndex(t *testing.T) {
	t.Parallel()
	persisted, generation := retrievalVectorFixture(t)
	changedContract, err := document.NewModelInputContract(document.ModelInputContractConfig{
		Profile: document.ModelInputProfileCustom, CompatibilityID: persisted.CompatibilityID,
		Document: persisted.ModelInput.Document,
		Query:    document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "changed: {{content}}"},
	})
	require.NoError(t, err)
	changed := persisted
	changed.ModelInput, changed.Fingerprint = changedContract, ""
	changed, err = document.NewEmbeddingDescriptor(changed)
	require.NoError(t, err)
	backend := &retrievalBackendStub{vaultID: "vault", authority: store.SemanticSearchAuthority{
		VectorSpace: store.EmbeddingVectorSpaceRecord{ID: generation.Metadata().VectorSpaceID, Descriptor: persisted},
		Lease: store.VectorIndexReaderLease{ID: "lease", FencingToken: 1,
			Generation: store.VectorIndexGenerationRecord{ID: strings.Repeat("4", 64),
				VectorSpaceID: generation.Metadata().VectorSpaceID, Bytes: generation.Bytes(),
				SourceManifestChecksum: strings.Repeat("c", 64),
				IndexManifestChecksum:  generation.Metadata().Manifest.Checksum,
				RowCount:               generation.Metadata().RowCount}},
		ScopedDocuments: 1, CompleteDocuments: 1, ANNRows: retrievalANNRows(t, generation)}}
	provider := &retrievalProvider{descriptor: changed, vector: []float32{1, 0}}
	searcher, err := NewSearcher(SearcherConfig{Backend: backend,
		Encoders: &retrievalResolver{provider: provider}, QueryEmbeddingAuthorizer: &retrievalQueryAuthorizer{},
		Owner:         "retrieval-test",
		LeaseDuration: time.Minute, Clock: time.Now})
	require.NoError(t, err)
	_, err = searcher.Search(t.Context(), Query{Text: "query", Mode: ModeSemantic, Limit: 1,
		ProcessingProfileFingerprint: strings.Repeat("a", 64), BindingID: "required",
		Authorization: retrievalAuthorization(persisted)})
	require.ErrorContains(t, err, "does not reproduce")
	assert.Zero(t, provider.calls)
	assert.Zero(t, backend.neighborCount)
}

func TestSearcherHybridUsesRRFWithoutComparingRawScores(t *testing.T) {
	t.Parallel()
	searcher, backend, _, descriptor := retrievalSearcherFixture(t, true, 1)
	backend.lexical = []store.ExplainedLexicalCandidate{{Node: store.Node{ID: 8,
		CurrentVersionID: "version-semantic", Name: "semantic.pdf"}, Path: "/semantic.pdf",
		Match: store.SearchMatchName, EvidenceKind: "node_name", Excerpt: "semantic.pdf"}}
	report, err := searcher.Search(t.Context(), Query{Text: "query", Mode: ModeHybrid, Limit: 3,
		ProcessingProfileFingerprint: strings.Repeat("a", 64), BindingID: "required",
		Authorization: retrievalAuthorization(descriptor)})
	require.NoError(t, err)
	assert.Equal(t, ModeHybrid, report.ActualMode)
	require.Len(t, report.Results, 1)
	assert.Equal(t, 1, report.Results[0].LexicalRank)
	assert.Equal(t, 1, report.Results[0].SemanticRank)
	assert.InDelta(t, 2.0/61.0, report.Results[0].Score, 1e-12)
}

func TestSearcherAutoDegradesRequiredCoverageBeforeProvider(t *testing.T) {
	t.Parallel()
	searcher, backend, provider, descriptor := retrievalSearcherFixture(t, true, 2)
	backend.lexical = []store.ExplainedLexicalCandidate{{Node: store.Node{ID: 5,
		CurrentVersionID: "version-lexical", Name: "lexical.pdf"}, Path: "/lexical.pdf",
		Match: store.SearchMatchName, EvidenceKind: "node_name", Excerpt: "lexical.pdf"}}
	report, err := searcher.Search(t.Context(), Query{Text: "query", Mode: ModeAuto, Limit: 3,
		ProcessingProfileFingerprint: strings.Repeat("a", 64), BindingID: "required",
		Authorization: retrievalAuthorization(descriptor)})
	require.NoError(t, err)
	assert.Equal(t, ModeLexical, report.ActualMode)
	assert.Equal(t, DegradationIncompleteCoverage, report.Degradation)
	assert.Zero(t, provider.calls)
}

func TestSearcherAutoDegradesProviderOutageButOptionalIncompleteCanHybrid(t *testing.T) {
	t.Parallel()
	t.Run("provider outage", func(t *testing.T) {
		searcher, backend, provider, descriptor := retrievalSearcherFixture(t, true, 1)
		provider.err = errors.New("synthetic outage")
		backend.lexical = []store.ExplainedLexicalCandidate{{Node: store.Node{ID: 5,
			CurrentVersionID: "version-lexical", Name: "lexical.pdf"}, Path: "/lexical.pdf",
			Match: store.SearchMatchName, EvidenceKind: "node_name", Excerpt: "lexical.pdf"}}
		report, err := searcher.Search(t.Context(), Query{Text: "query", Mode: ModeAuto, Limit: 3,
			ProcessingProfileFingerprint: strings.Repeat("a", 64), BindingID: "required",
			Authorization: retrievalAuthorization(descriptor)})
		require.NoError(t, err)
		assert.Equal(t, ModeLexical, report.ActualMode)
		assert.Equal(t, DegradationProviderUnavailable, report.Degradation)
	})
	t.Run("optional incomplete", func(t *testing.T) {
		searcher, _, provider, descriptor := retrievalSearcherFixture(t, false, 2)
		report, err := searcher.Search(t.Context(), Query{Text: "query", Mode: ModeAuto, Limit: 3,
			ProcessingProfileFingerprint: strings.Repeat("a", 64), BindingID: "optional",
			Authorization: retrievalAuthorization(descriptor)})
		require.NoError(t, err)
		assert.Equal(t, ModeHybrid, report.ActualMode)
		assert.Equal(t, CoverageIncomplete, report.Coverage.State)
		assert.Equal(t, 1, provider.calls)
	})
}

func TestSearcherAutoPropagatesCorruptLeasedIndex(t *testing.T) {
	t.Parallel()
	searcher, backend, _, descriptor := retrievalSearcherFixture(t, true, 1)
	backend.authority.Lease.Generation.Bytes = []byte("corrupt-index")
	backend.lexical = []store.ExplainedLexicalCandidate{{Node: store.Node{ID: 5,
		CurrentVersionID: "version-lexical", Name: "lexical.pdf"}, Path: "/lexical.pdf",
		Match: store.SearchMatchName, EvidenceKind: "node_name", Excerpt: "lexical.pdf"}}

	_, err := searcher.Search(t.Context(), Query{Text: "query", Mode: ModeAuto, Limit: 3,
		ProcessingProfileFingerprint: strings.Repeat("a", 64), BindingID: "required",
		Authorization: retrievalAuthorization(descriptor)})

	require.Error(t, err)
	assert.Zero(t, backend.lexicalCalls, "local index corruption must not be hidden by lexical degradation")
}

func TestSearcherAutoPropagatesLeaseReleaseFailure(t *testing.T) {
	t.Parallel()
	searcher, backend, provider, descriptor := retrievalSearcherFixture(t, true, 1)
	provider.err = errors.New("synthetic provider outage")
	backend.releaseErr = errors.New("synthetic lease release failure")
	backend.lexical = []store.ExplainedLexicalCandidate{{Node: store.Node{ID: 5,
		CurrentVersionID: "version-lexical", Name: "lexical.pdf"}, Path: "/lexical.pdf",
		Match: store.SearchMatchName, EvidenceKind: "node_name", Excerpt: "lexical.pdf"}}

	_, err := searcher.Search(t.Context(), Query{Text: "query", Mode: ModeAuto, Limit: 3,
		ProcessingProfileFingerprint: strings.Repeat("a", 64), BindingID: "required",
		Authorization: retrievalAuthorization(descriptor)})

	require.ErrorContains(t, err, "lease release failure")
	assert.Zero(t, backend.lexicalCalls, "a lease failure must not be hidden by lexical degradation")
}

func TestSearcherReleasesLeaseAfterCallerCancellation(t *testing.T) {
	t.Parallel()
	searcher, backend, _, descriptor := retrievalSearcherFixture(t, true, 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := searcher.Search(ctx, Query{Text: "query", Mode: ModeSemantic, Limit: 3,
		ProcessingProfileFingerprint: strings.Repeat("a", 64), BindingID: "required",
		Authorization: retrievalAuthorization(descriptor)})

	require.NoError(t, err)
	assert.NoError(t, backend.releaseContextErr)
}

func TestSearcherRejectsWrongStoredIndexManifestBeforeSearch(t *testing.T) {
	t.Parallel()
	searcher, backend, _, descriptor := retrievalSearcherFixture(t, true, 1)
	backend.authority.Lease.Generation.IndexManifestChecksum = strings.Repeat("d", 64)

	_, err := searcher.Search(t.Context(), Query{Text: "query", Mode: ModeSemantic, Limit: 3,
		ProcessingProfileFingerprint: strings.Repeat("a", 64), BindingID: "required",
		Authorization: retrievalAuthorization(descriptor)})

	require.ErrorContains(t, err, "leased vector generation is incompatible")
	assert.Zero(t, backend.neighborCount)
}

func TestSearcherAutoDegradesAbsentSemanticAuthority(t *testing.T) {
	t.Parallel()
	searcher, backend, _, descriptor := retrievalSearcherFixture(t, true, 1)
	backend.acquireErr = store.ErrNotFound
	backend.lexical = []store.ExplainedLexicalCandidate{{Node: store.Node{ID: 5,
		CurrentVersionID: "version-lexical", Name: "lexical.pdf"}, Path: "/lexical.pdf",
		Match: store.SearchMatchName, EvidenceKind: "node_name", Excerpt: "lexical.pdf"}}

	report, err := searcher.Search(t.Context(), Query{Text: "query", Mode: ModeAuto, Limit: 3,
		ProcessingProfileFingerprint: strings.Repeat("a", 64), BindingID: "required",
		Authorization: retrievalAuthorization(descriptor)})

	require.NoError(t, err)
	assert.Equal(t, ModeLexical, report.ActualMode)
	assert.Equal(t, DegradationSemanticUnavailable, report.Degradation)
}

func TestSearcherAutoDegradesWhenFinalCoverageBecomesIncomplete(t *testing.T) {
	t.Parallel()
	searcher, backend, _, descriptor := retrievalSearcherFixture(t, true, 1)
	backend.finalScopedDocuments = 2
	backend.finalCompleteDocuments = 1
	backend.finalCoverageSet = true
	backend.lexical = []store.ExplainedLexicalCandidate{{Node: store.Node{ID: 5,
		CurrentVersionID: "version-lexical", Name: "lexical.pdf"}, Path: "/lexical.pdf",
		Match: store.SearchMatchName, EvidenceKind: "node_name", Excerpt: "lexical.pdf"}}

	report, err := searcher.Search(t.Context(), Query{Text: "query", Mode: ModeAuto, Limit: 3,
		ProcessingProfileFingerprint: strings.Repeat("a", 64), BindingID: "required",
		Authorization: retrievalAuthorization(descriptor)})

	require.NoError(t, err)
	assert.Equal(t, ModeLexical, report.ActualMode)
	assert.Equal(t, DegradationIncompleteCoverage, report.Degradation)
	assert.Equal(t, 2, report.Coverage.ScopedDocuments)
	assert.Equal(t, 1, report.Coverage.CompleteDocuments)
}

func retrievalSearcherFixture(t *testing.T, required bool, scoped int) (
	*Searcher, *retrievalBackendStub, *retrievalProvider, document.EmbeddingDescriptor,
) {
	t.Helper()
	descriptor, generation := retrievalVectorFixture(t)
	backend := &retrievalBackendStub{vaultID: "vault", authority: store.SemanticSearchAuthority{
		VectorSpace: store.EmbeddingVectorSpaceRecord{ID: generation.Metadata().VectorSpaceID, Descriptor: descriptor},
		Lease: store.VectorIndexReaderLease{ID: "lease", FencingToken: 1,
			Generation: store.VectorIndexGenerationRecord{ID: strings.Repeat("5", 64),
				VectorSpaceID: generation.Metadata().VectorSpaceID, Bytes: generation.Bytes(),
				SourceManifestChecksum: strings.Repeat("c", 64),
				IndexManifestChecksum:  generation.Metadata().Manifest.Checksum,
				RowCount:               generation.Metadata().RowCount}},
		BindingRequired: required,
		ScopedDocuments: scoped, CompleteDocuments: 1, ANNRows: retrievalANNRows(t, generation),
	}, semantic: []store.SemanticSearchCandidate{{VaultID: "vault", NodeID: 8,
		ContentVersionID: "version-semantic", Path: "/semantic.pdf",
		VectorSpaceID: generation.Metadata().VectorSpaceID, EmbeddingSetID: "set",
		InputGenerationID: "generation", InputID: "input",
		InputKind: document.EmbeddingInputOriginalFile, Score: 999}}}
	provider := &retrievalProvider{descriptor: descriptor, vector: []float32{1, 0}}
	searcher, err := NewSearcher(SearcherConfig{Backend: backend,
		Encoders: &retrievalResolver{provider: provider}, QueryEmbeddingAuthorizer: &retrievalQueryAuthorizer{},
		Owner:         "retrieval-test",
		LeaseDuration: time.Minute, Clock: time.Now})
	require.NoError(t, err)
	return searcher, backend, provider, descriptor
}

func retrievalANNRows(t *testing.T, generation *vectorindex.Generation) []vectorindex.RowIdentity {
	t.Helper()
	metadata := generation.Metadata()
	neighbors, err := generation.Search([]float32{1, 0}, metadata.RowCount, metadata.RowCount)
	require.NoError(t, err)
	rows := make([]vectorindex.RowIdentity, len(neighbors))
	for index, neighbor := range neighbors {
		rows[index] = neighbor.RowIdentity
	}
	return rows
}

type retrievalBackendStub struct {
	vaultID                string
	lexical                []store.ExplainedLexicalCandidate
	authority              store.SemanticSearchAuthority
	semantic               []store.SemanticSearchCandidate
	acquireErr             error
	neighborCount          int
	sourceManifest         string
	acquiredAt             time.Time
	releasedAt             time.Time
	lexicalCalls           int
	releaseErr             error
	releaseContextErr      error
	finalScopedDocuments   int
	finalCompleteDocuments int
	finalCoverageSet       bool
}

func (backend *retrievalBackendStub) AcquireSemanticSearchAuthority(_ context.Context, _, _, _ string,
	at time.Time, _ time.Duration, _ store.SearchOptions,
) (store.SemanticSearchAuthority, error) {
	backend.acquiredAt = at
	return backend.authority, backend.acquireErr
}

func (backend *retrievalBackendStub) ResolveSemanticCandidates(_ context.Context, _, _ string,
	_ document.EmbeddingInputKind, _, sourceManifest string, neighbors []vectorindex.Neighbor,
	_ int, _ store.SearchOptions,
) (store.SemanticSearchResolution, error) {
	backend.neighborCount = len(neighbors)
	backend.sourceManifest = sourceManifest
	scoped, complete := backend.finalScopedDocuments, backend.finalCompleteDocuments
	if !backend.finalCoverageSet {
		scoped, complete = backend.authority.ScopedDocuments, backend.authority.CompleteDocuments
	}
	return store.SemanticSearchResolution{
		SourceManifestChecksum: sourceManifest,
		Candidates:             append([]store.SemanticSearchCandidate(nil), backend.semantic...),
		ScopedDocuments:        scoped, CompleteDocuments: complete,
	}, nil
}

func (backend *retrievalBackendStub) ReleaseVectorIndexGeneration(ctx context.Context, _ string,
	_ int64, at time.Time,
) error {
	backend.releasedAt = at
	backend.releaseContextErr = ctx.Err()
	return backend.releaseErr
}

type retrievalResolver struct {
	provider  document.EmbeddingProvider
	err       error
	requested document.EmbeddingDescriptor
}

type retrievalQueryAuthorizer struct {
	err        error
	operations []QueryEmbeddingOperation
}

func (authorizer *retrievalQueryAuthorizer) AuthorizeQueryEmbedding(
	_ context.Context, operation QueryEmbeddingOperation,
) error {
	authorizer.operations = append(authorizer.operations, operation)
	return authorizer.err
}

func (resolver *retrievalResolver) ResolveQueryEncoder(_ context.Context,
	descriptor document.EmbeddingDescriptor,
) (document.EmbeddingProvider, error) {
	resolver.requested = descriptor
	return resolver.provider, resolver.err
}

type retrievalProvider struct {
	descriptor document.EmbeddingDescriptor
	vector     []float32
	err        error
	calls      int
	rendered   string
}

func (provider *retrievalProvider) Descriptor() document.EmbeddingDescriptor {
	return provider.descriptor
}

func (provider *retrievalProvider) Embed(_ context.Context, inputs []document.EmbeddingInput,
	_ document.EmbeddingAuthorization,
) (document.EmbeddingResult, error) {
	provider.calls++
	provider.rendered = provider.descriptor.ModelInput.EncodeQuery(inputs[0].Text)
	if provider.err != nil {
		return document.EmbeddingResult{}, provider.err
	}
	return document.EmbeddingResult{Vectors: []document.EmbeddingVector{{
		Key: inputs[0].Key, Values: append([]float32(nil), provider.vector...),
	}}}, nil
}

func retrievalAuthorization(descriptor document.EmbeddingDescriptor) document.EmbeddingAuthorization {
	return document.EmbeddingAuthorization{ProviderID: descriptor.ID,
		DescriptorFingerprint: descriptor.Fingerprint, PolicyFingerprint: descriptor.PolicyFingerprint,
		MaxBatchItems: 1, MaxInputBytes: 1 << 20, MaxResponseBytes: 1 << 20}
}

func retrievalVectorFixture(t *testing.T) (document.EmbeddingDescriptor, *vectorindex.Generation) {
	t.Helper()
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{
		Profile: document.ModelInputProfileCustom, CompatibilityID: "retrieval-test-space",
		Document: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "document: {{content}}"},
		Query:    document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "query: {{content}}"},
	})
	require.NoError(t, err)
	descriptor, err := document.NewEmbeddingDescriptor(document.EmbeddingDescriptor{
		ID: "retrieval-test", ContractVersion: document.EmbeddingProviderContractVersion,
		PolicyFingerprint: strings.Repeat("a", 64), TrustBoundary: document.EmbeddingTrustLocalProcess,
		Model: "synthetic", ModelRevision: "v1", Dimension: 2, Metric: document.VectorMetricCosine,
		Normalization: document.VectorNormalizationUnitLength, ScalarEncoding: "float32",
		DocumentFormatter: "document/v1", QueryFormatter: "query/v1",
		InputKinds:      []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile},
		CompatibilityID: contract.CompatibilityID, SupportsTextQuery: true, ModelInput: contract,
		SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeText},
	})
	require.NoError(t, err)
	space := strings.Repeat("b", 64)
	set := document.VectorSetV1{VectorSpaceFingerprint: space, Metric: descriptor.Metric,
		Normalization: descriptor.Normalization, Dimension: 2,
		InputKeys:      []string{"one", "two", "three"},
		InputChecksums: []string{strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64)},
		Vectors:        [][]float32{{1, 0}, {0, 1}, {-1, 0}}}
	_, setID, err := document.EncodeVectorSetV1(set)
	require.NoError(t, err)
	manifest, err := vectorindex.NewManifest([]string{setID})
	require.NoError(t, err)
	generation, err := vectorindex.BuildGeneration(manifest, []document.VectorSetV1{set}, vectorindex.Options{})
	require.NoError(t, err)
	opened, err := vectorindex.OpenGeneration(bytes.NewReader(generation.Bytes()), int64(len(generation.Bytes())))
	require.NoError(t, err)
	return descriptor, opened
}

func (backend *retrievalBackendStub) VaultID() string { return backend.vaultID }

func (backend *retrievalBackendStub) SearchExplainedLexicalCandidates(context.Context, string, int,
	store.SearchOptions,
) ([]store.ExplainedLexicalCandidate, bool, error) {
	backend.lexicalCalls++
	return append([]store.ExplainedLexicalCandidate(nil), backend.lexical...), false, nil
}

func TestFuseReciprocalRankPreservesExactLaneContributions(t *testing.T) {
	t.Parallel()

	shared := DocumentIdentity{VaultID: "vault", NodeID: 7, ContentVersionID: "version-a"}
	lexicalOnly := DocumentIdentity{VaultID: "vault", NodeID: 13, ContentVersionID: "version-b"}
	semanticOnly := DocumentIdentity{VaultID: "vault", NodeID: 9, ContentVersionID: "version-c"}
	results, truncated, err := FuseReciprocalRank([]Candidate{
		{Document: shared, Lane: LaneLexical, Rank: 1},
		{Document: lexicalOnly, Lane: LaneLexical, Rank: 2},
	}, []Candidate{
		{Document: semanticOnly, Lane: LaneSemantic, Rank: 1, VectorSpaceID: "space"},
		{Document: shared, Lane: LaneSemantic, Rank: 2, VectorSpaceID: "space"},
	}, 3)
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, results, 3)
	assert.Equal(t, shared, results[0].Document)
	assert.Equal(t, 1, results[0].LexicalRank)
	assert.Equal(t, 2, results[0].SemanticRank)
	assert.InDelta(t, 1.0/61.0, results[0].Explanation[0].Contribution, 1e-12)
	assert.InDelta(t, 1.0/62.0, results[0].Explanation[1].Contribution, 1e-12)
	assert.Equal(t, semanticOnly, results[1].Document,
		"equal RRF scores use stable document identity, never raw lane score")
}

func TestFuseReciprocalRankRejectsMixedSemanticVectorSpaces(t *testing.T) {
	t.Parallel()

	_, _, err := FuseReciprocalRank(nil, []Candidate{
		{Document: DocumentIdentity{VaultID: "vault", NodeID: 1, ContentVersionID: "version-a"},
			Lane: LaneSemantic, Rank: 1, VectorSpaceID: "space-a",
			Evidence: []EvidenceReference{{Kind: "embedding", VectorSpaceID: "space-a"}}},
		{Document: DocumentIdentity{VaultID: "vault", NodeID: 2, ContentVersionID: "version-b"},
			Lane: LaneSemantic, Rank: 2, VectorSpaceID: "space-b",
			Evidence: []EvidenceReference{{Kind: "embedding", VectorSpaceID: "space-b"}}},
	}, 2)
	require.ErrorContains(t, err, "one active vector space")
}
