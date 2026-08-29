package processing

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/retrieval"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorindex"
)

func TestConsumerSourceFence(t *testing.T) {
	// Mutation caught: passing an unfenced scope to any retrieval/provider stage
	// would admit the revoked source version to an external consumer.
	allowed := retrieval.DocumentIdentity{VaultID: "123e4567-e89b-42d3-a456-426614174001", NodeID: 41,
		ContentVersionID: "123e4567-e89b-42d3-a456-426614174002"}
	revoked := retrieval.DocumentIdentity{VaultID: allowed.VaultID, NodeID: 42,
		ContentVersionID: "123e4567-e89b-42d3-a456-426614174003"}
	consumer := func(attachment []byte) externalConsumer {
		mirror := append([]byte(nil), attachment...)
		require.Equal(t, attachment, mirror)
		return externalConsumer{document: allowed, fence: store.SearchOptions{
			ContentVersionIDs: []string{allowed.ContentVersionID},
		}}
	}([]byte("synthetic external-consumer attachment bytes"))

	source := newConsumerSource(t, consumer, revoked)
	source.revoke(revoked.ContentVersionID)
	searcher := newConsumerSearcher(t, source)

	report, err := searcher.Search(t.Context(), retrieval.Query{Text: "synthetic query", Mode: retrieval.ModeHybrid,
		Limit: 10, Scope: consumer.fence, ProcessingProfileFingerprint: strings.Repeat("d", 64),
		BindingID: "synthetic", Authorization: consumerEmbeddingAuthorization(source.descriptor)})
	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	assert.Equal(t, allowed, report.Results[0].Document)
	assert.Equal(t, []retrieval.DocumentIdentity{allowed, revoked}, source.corpusIdentities())
	assert.Equal(t, []retrieval.DocumentIdentity{allowed, allowed}, source.lexicalCandidates)
	assert.Equal(t, []string{"synthetic-allowed", "synthetic-allowed"}, source.annNeighborInputs,
		"the source fence must remove revoked rows from ANN input before scoring")
	assert.Equal(t, []retrieval.DocumentIdentity{allowed, allowed}, source.vectorCandidates)
	assert.Equal(t, []retrieval.DocumentIdentity{allowed}, source.rerankCandidates)
	assert.NotContains(t, source.observed, revoked,
		"a source version absent from the prefilter must not enter lexical, vector, or reranking work")
	assert.Equal(t, []string{
		"expansion_authorize", "expansion",
		"lexical", "vector_authority", "vector_authorize", "vector_embed", "vector_candidates",
		"lexical", "vector_authority", "vector_authorize", "vector_embed", "vector_candidates",
		"final_revalidate", "rerank_authorize", "rerank",
	}, source.stages, "the source fence must reach every stage before candidate work")

	// The final visibility check is deliberately at presentation time: revocation
	// after all retrieval/provider work must still suppress the stale result.
	source.revoke(allowed.ContentVersionID)
	presented := consumer.present(report, source.live)
	assert.Empty(t, presented)
}

type externalConsumer struct {
	// This is the complete durable external-consumer identity: no mirrored bytes,
	// paths, excerpts, or provider evidence are retained across the boundary.
	document retrieval.DocumentIdentity
	fence    store.SearchOptions
}

func (consumer externalConsumer) present(report retrieval.Report, live map[string]bool) []retrieval.Result {
	results := make([]retrieval.Result, 0, len(report.Results))
	for _, result := range report.Results {
		if result.Document == consumer.document &&
			slices.Contains(consumer.fence.ContentVersionIDs, result.Document.ContentVersionID) &&
			live[result.Document.ContentVersionID] {
			results = append(results, result)
		}
	}
	return results
}

type consumerSource struct {
	consumer  externalConsumer
	revoked   retrieval.DocumentIdentity
	documents []consumerCorpusDocument
	live      map[string]bool

	descriptor        document.EmbeddingDescriptor
	authority         store.SemanticSearchAuthority
	observed          []retrieval.DocumentIdentity
	lexicalCandidates []retrieval.DocumentIdentity
	annNeighborInputs []string
	vectorCandidates  []retrieval.DocumentIdentity
	rerankCandidates  []retrieval.DocumentIdentity
	stages            []string
}

type consumerCorpusDocument struct {
	document      retrieval.DocumentIdentity
	name, inputID string
	annRow        vectorindex.RowIdentity
}

func newConsumerSource(t *testing.T, consumer externalConsumer, revoked retrieval.DocumentIdentity) *consumerSource {
	t.Helper()
	documents := []consumerCorpusDocument{
		{document: consumer.document, name: "allowed.pdf", inputID: "synthetic-allowed"},
		{document: revoked, name: "revoked.pdf", inputID: "synthetic-revoked"},
	}
	descriptor, generation := consumerVectorFixture(t, documents)
	neighbors, err := generation.Search([]float32{1, 0}, len(documents), len(documents))
	require.NoError(t, err)
	for index := range documents {
		for _, neighbor := range neighbors {
			if neighbor.InputKey == documents[index].inputID {
				documents[index].annRow = neighbor.RowIdentity
				break
			}
		}
	}
	return &consumerSource{consumer: consumer, revoked: revoked, documents: documents,
		live:       map[string]bool{consumer.document.ContentVersionID: true, revoked.ContentVersionID: true},
		descriptor: descriptor,
		authority: store.SemanticSearchAuthority{VectorSpace: store.EmbeddingVectorSpaceRecord{
			ID: generation.Metadata().VectorSpaceID, Descriptor: descriptor},
			Lease: store.VectorIndexReaderLease{ID: "synthetic-lease", FencingToken: 1,
				Generation: store.VectorIndexGenerationRecord{ID: strings.Repeat("5", 64),
					VectorSpaceID: generation.Metadata().VectorSpaceID, Bytes: generation.Bytes(),
					SourceManifestChecksum: strings.Repeat("c", 64),
					IndexManifestChecksum:  generation.Metadata().Manifest.Checksum,
					RowCount:               generation.Metadata().RowCount}},
			BindingRequired: true, ScopedDocuments: 1, CompleteDocuments: 1},
	}
}

func (source *consumerSource) revoke(versionID string) { source.live[versionID] = false }

func (source *consumerSource) VaultID() string { return source.consumer.document.VaultID }

func (source *consumerSource) SearchExplainedLexicalCandidates(_ context.Context, _ string, _ int,
	scope store.SearchOptions,
) ([]store.ExplainedLexicalCandidate, bool, error) {
	source.stages = append(source.stages, "lexical")
	documents, err := source.fencedCorpus(scope)
	if err != nil {
		return nil, false, err
	}
	candidates := make([]store.ExplainedLexicalCandidate, 0, len(documents))
	for _, candidate := range documents {
		source.observed = append(source.observed, candidate.document)
		source.lexicalCandidates = append(source.lexicalCandidates, candidate.document)
		candidates = append(candidates, store.ExplainedLexicalCandidate{Node: store.Node{ID: candidate.document.NodeID,
			CurrentVersionID: candidate.document.ContentVersionID, Name: candidate.name, Revision: 1},
			Path: "/" + candidate.name, EvidenceKind: "node_name", Excerpt: "synthetic evidence"})
	}
	return candidates, false, nil
}

func (source *consumerSource) AcquireSemanticSearchAuthority(_ context.Context, _, _, _ string, _ time.Time,
	_ time.Duration, scope store.SearchOptions,
) (store.SemanticSearchAuthority, error) {
	source.stages = append(source.stages, "vector_authority")
	if err := source.requireFence(scope); err != nil {
		return store.SemanticSearchAuthority{}, err
	}
	documents, err := source.fencedCorpus(scope)
	if err != nil {
		return store.SemanticSearchAuthority{}, err
	}
	authority := source.authority
	authority.ANNRows = make([]vectorindex.RowIdentity, 0, len(documents))
	for _, candidate := range documents {
		authority.ANNRows = append(authority.ANNRows, candidate.annRow)
	}
	return authority, nil
}

func (source *consumerSource) ResolveSemanticCandidates(_ context.Context, _, _ string,
	_ document.EmbeddingInputKind, _ string, sourceManifest string, neighbors []vectorindex.Neighbor, _ int,
	scope store.SearchOptions,
) (store.SemanticSearchResolution, error) {
	source.stages = append(source.stages, "vector_candidates")
	for _, neighbor := range neighbors {
		source.annNeighborInputs = append(source.annNeighborInputs, neighbor.InputKey)
	}
	documents, err := source.fencedCorpus(scope)
	if err != nil {
		return store.SemanticSearchResolution{}, err
	}
	byInput := make(map[string]consumerCorpusDocument, len(documents))
	for _, candidate := range documents {
		byInput[candidate.inputID] = candidate
	}
	resolution := store.SemanticSearchResolution{SourceManifestChecksum: sourceManifest, ScopedDocuments: len(documents),
		CompleteDocuments: len(documents)}
	for _, neighbor := range neighbors {
		candidate, allowed := byInput[neighbor.InputKey]
		if !allowed {
			continue
		}
		source.observed = append(source.observed, candidate.document)
		source.vectorCandidates = append(source.vectorCandidates, candidate.document)
		resolution.Candidates = append(resolution.Candidates, store.SemanticSearchCandidate{
			VaultID: candidate.document.VaultID, NodeID: candidate.document.NodeID, NodeRevision: 1,
			ContentVersionID: candidate.document.ContentVersionID, Path: "/" + candidate.name,
			VectorSpaceID: source.authority.VectorSpace.ID, EmbeddingSetID: "synthetic-set",
			InputGenerationID: "synthetic-generation", InputID: candidate.inputID,
			InputKind: document.EmbeddingInputOriginalFile, Score: neighbor.Score,
		})
	}
	return resolution, nil
}

func (source *consumerSource) ReleaseVectorIndexGeneration(context.Context, string, int64, time.Time) error {
	return nil
}

func (source *consumerSource) RevalidateSearchCandidates(_ context.Context, candidates []store.SearchCandidateIdentity,
	scope store.SearchOptions, _, _ string,
) (store.SearchCandidateRevalidation, error) {
	source.stages = append(source.stages, "final_revalidate")
	documents, err := source.fencedCorpus(scope)
	if err != nil {
		return store.SearchCandidateRevalidation{}, err
	}
	allowedDocuments := make(map[retrieval.DocumentIdentity]struct{}, len(documents))
	for _, document := range documents {
		allowedDocuments[document.document] = struct{}{}
	}
	allowed := make([]store.SearchCandidateIdentity, 0, len(candidates))
	for _, candidate := range candidates {
		document := retrieval.DocumentIdentity{VaultID: source.consumer.document.VaultID,
			NodeID: candidate.NodeID, ContentVersionID: candidate.ContentVersionID}
		if _, current := allowedDocuments[document]; current &&
			source.live[candidate.ContentVersionID] {
			allowed = append(allowed, candidate)
		}
	}
	return store.SearchCandidateRevalidation{Candidates: allowed}, nil
}

func (source *consumerSource) AuthorizeQueryEmbedding(_ context.Context,
	operation retrieval.QueryEmbeddingOperation,
) error {
	source.stages = append(source.stages, "vector_authorize")
	return source.requireFence(operation.Scope)
}

func (source *consumerSource) AuthorizeExpansion(_ context.Context, operation retrieval.ProviderOperation) error {
	source.stages = append(source.stages, "expansion_authorize")
	return source.requireFence(operation.Scope)
}

func (source *consumerSource) AuthorizeReranking(_ context.Context, operation retrieval.ProviderOperation) error {
	source.stages = append(source.stages, "rerank_authorize")
	if err := source.requireFence(operation.Scope); err != nil {
		return err
	}
	if slices.Contains(source.observed, source.revoked) {
		return errors.New("revoked document reached reranking authorization")
	}
	return nil
}

func (source *consumerSource) requireFence(scope store.SearchOptions) error {
	if !slices.Equal(scope.ContentVersionIDs, source.consumer.fence.ContentVersionIDs) {
		return errors.New("source fence was not applied before retrieval work")
	}
	return nil
}

func (source *consumerSource) fencedCorpus(scope store.SearchOptions) ([]consumerCorpusDocument, error) {
	if err := source.requireFence(scope); err != nil {
		return nil, err
	}
	selected := make([]consumerCorpusDocument, 0, len(source.documents))
	for _, candidate := range source.documents {
		if slices.Contains(scope.ContentVersionIDs, candidate.document.ContentVersionID) {
			selected = append(selected, candidate)
		}
	}
	return selected, nil
}

func (source *consumerSource) corpusIdentities() []retrieval.DocumentIdentity {
	identities := make([]retrieval.DocumentIdentity, len(source.documents))
	for index, candidate := range source.documents {
		identities[index] = candidate.document
	}
	return identities
}

type consumerResolver struct{ provider document.EmbeddingProvider }

func (resolver consumerResolver) ResolveQueryEncoder(context.Context, document.EmbeddingDescriptor) (document.EmbeddingProvider, error) {
	return resolver.provider, nil
}

type consumerEmbedder struct {
	descriptor document.EmbeddingDescriptor
	source     *consumerSource
}

func (embedder consumerEmbedder) Descriptor() document.EmbeddingDescriptor {
	return embedder.descriptor
}

func (embedder consumerEmbedder) Embed(context.Context, []document.EmbeddingInput,
	document.EmbeddingAuthorization,
) (document.EmbeddingResult, error) {
	embedder.source.stages = append(embedder.source.stages, "vector_embed")
	return document.EmbeddingResult{Vectors: []document.EmbeddingVector{{Key: "query", Values: []float32{1, 0}}}}, nil
}

type consumerExpander struct{ source *consumerSource }

func (expander consumerExpander) Expand(context.Context, retrieval.ExpansionRequest) ([]string, error) {
	expander.source.stages = append(expander.source.stages, "expansion")
	return []string{"synthetic expanded query"}, nil
}

type consumerReranker struct{ source *consumerSource }

func (reranker consumerReranker) Rerank(_ context.Context, request retrieval.RerankingRequest) ([]retrieval.RerankScore, error) {
	reranker.source.stages = append(reranker.source.stages, "rerank")
	for _, candidate := range request.Candidates {
		reranker.source.observed = append(reranker.source.observed, candidate.Document)
		reranker.source.rerankCandidates = append(reranker.source.rerankCandidates, candidate.Document)
	}
	scores := make([]retrieval.RerankScore, len(request.Candidates))
	for index, candidate := range request.Candidates {
		scores[index] = retrieval.RerankScore{Document: candidate.Document, Score: 1}
	}
	return scores, nil
}

func newConsumerSearcher(t *testing.T, source *consumerSource) *retrieval.Searcher {
	t.Helper()
	searcher, err := retrieval.NewSearcher(retrieval.SearcherConfig{Backend: source,
		Encoders:                 consumerResolver{provider: consumerEmbedder{descriptor: source.descriptor, source: source}},
		QueryEmbeddingAuthorizer: source, Owner: "synthetic-external-consumer", LeaseDuration: time.Minute,
		Clock: time.Now, Expansion: retrieval.ExpansionConfig{Enabled: true,
			Profile:  retrieval.ExpansionProfile{ID: "synthetic-expansion", MaxVariants: 1},
			Provider: consumerExpander{source: source}, Authorizer: source, Deadline: time.Second,
			FailurePolicy: retrieval.ProviderFailureFailClosed},
		Reranking: retrieval.RerankingConfig{Enabled: true,
			Profile:  retrieval.RerankingProfile{ID: "synthetic-reranking", MaxCandidates: 10},
			Provider: consumerReranker{source: source}, Authorizer: source, Deadline: time.Second,
			FailurePolicy: retrieval.ProviderFailureFailClosed}})
	require.NoError(t, err)
	return searcher
}

func consumerEmbeddingAuthorization(descriptor document.EmbeddingDescriptor) document.EmbeddingAuthorization {
	return document.EmbeddingAuthorization{ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
		PolicyFingerprint: descriptor.PolicyFingerprint, MaxBatchItems: 1, MaxInputBytes: 1 << 20,
		MaxResponseBytes: 1 << 20}
}

func consumerVectorFixture(t *testing.T, documents []consumerCorpusDocument) (document.EmbeddingDescriptor, *vectorindex.Generation) {
	t.Helper()
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{
		Profile: document.ModelInputProfileCustom, CompatibilityID: "synthetic-consumer-space",
		Document: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "document: {{content}}"},
		Query:    document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "query: {{content}}"},
	})
	require.NoError(t, err)
	descriptor, err := document.NewEmbeddingDescriptor(document.EmbeddingDescriptor{ID: "synthetic-consumer",
		ContractVersion: document.EmbeddingProviderContractVersion, PolicyFingerprint: strings.Repeat("a", 64),
		TrustBoundary: document.EmbeddingTrustLocalProcess, Model: "synthetic", ModelRevision: "v1",
		Dimension: 2, Metric: document.VectorMetricCosine, Normalization: document.VectorNormalizationUnitLength,
		ScalarEncoding: "float32", DocumentFormatter: "document/v1", QueryFormatter: "query/v1",
		InputKinds:      []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile},
		CompatibilityID: contract.CompatibilityID, SupportsTextQuery: true, ModelInput: contract,
		SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeText}})
	require.NoError(t, err)
	inputKeys := make([]string, len(documents))
	inputChecksums := make([]string, len(documents))
	vectors := make([][]float32, len(documents))
	for index, candidate := range documents {
		inputKeys[index] = candidate.inputID
		inputChecksums[index] = strings.Repeat(string(rune('1'+index)), 64)
		vectors[index] = []float32{1 - float32(index), float32(index)}
	}
	set := document.VectorSetV1{VectorSpaceFingerprint: strings.Repeat("b", 64), Metric: descriptor.Metric,
		Normalization: descriptor.Normalization, Dimension: 2, InputKeys: inputKeys,
		InputChecksums: inputChecksums, Vectors: vectors}
	_, setID, err := document.EncodeVectorSetV1(set)
	require.NoError(t, err)
	manifest, err := vectorindex.NewManifest([]string{setID})
	require.NoError(t, err)
	built, err := vectorindex.BuildGeneration(manifest, []document.VectorSetV1{set}, vectorindex.Options{})
	require.NoError(t, err)
	generation, err := vectorindex.OpenGeneration(bytes.NewReader(built.Bytes()), int64(len(built.Bytes())))
	require.NoError(t, err)
	return descriptor, generation
}
