package retrieval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

func TestMediaTimeSpanPreservesZeroAndRejectsInventedTiming(t *testing.T) {
	span, err := mediaTimeSpan(document.EvidenceLocatorV1{
		Kind:        document.EvidenceLocatorSegment,
		IndexOrigin: document.EvidenceIndexOriginZero,
		Start:       0,
		End:         1000,
	})
	require.NoError(t, err)
	raw, err := json.Marshal(span)
	require.NoError(t, err)
	require.JSONEq(t, `{"start_ms":0,"end_ms":1000}`, string(raw))

	span, err = mediaTimeSpan(document.EvidenceLocatorV1{Kind: document.EvidenceLocatorGeneric})
	require.NoError(t, err)
	require.Nil(t, span)
}

func TestSearchSkipsUnavailableMediaEvidence(t *testing.T) {
	for name, blobs := range map[string]MediaEvidenceBlobReader{
		"missing": missingMediaBlobs{},
		"corrupt": &mediaBlobFixture{data: map[string][]byte{"missing": bytes.Repeat([]byte("x"), 100)}},
	} {
		t.Run(name, func(t *testing.T) {
			searcher, backend, _, descriptor := retrievalSearcherFixture(t, true, 2)
			searcher.mediaEvidence = NewMediaEvidenceResolver(blobs)
			broken := backend.semantic[0]
			broken.NodeID = 9
			broken.ContentVersionID = "unavailable-version"
			broken.InputKind = document.EmbeddingInputRenditionChunk
			broken.MediaEvidence = store.SearchMediaEvidence{BuildID: "build", GenerationBlobHash: "missing",
				GenerationEncodedSize: 100, GenerationChecksum: "checksum", EvidenceFingerprint: "evidence",
				EvidenceEncodedSize: 100, InputCount: 1}
			backend.semanticPages = []store.SemanticSearchResolution{
				{Candidates: []store.SemanticSearchCandidate{broken}, Truncated: true, NextNeighbor: 2},
				{Candidates: backend.semantic, NextNeighbor: 1},
			}
			report, err := searcher.Search(t.Context(), Query{Text: "query", Mode: ModeSemantic, Limit: 1,
				ProcessingProfileFingerprint: strings.Repeat("a", 64), BindingID: "required",
				Authorization: retrievalAuthorization(descriptor)})
			require.NoError(t, err)
			require.Len(t, report.Results, 1)
			require.Equal(t, "version-semantic", report.Results[0].Document.ContentVersionID)
			require.Equal(t, 1, report.Results[0].Rank)
			require.True(t, report.Truncated)
			require.Empty(t, backend.semanticPages)
			require.Contains(t, backend.excludedNodes, broken.NodeID)
			require.Equal(t, 1, backend.neighborCount, "refill advances through the leased neighbors")
		})
	}
}

func TestLexicalTimingDoesNotNeedArtifactReadsOrRevalidation(t *testing.T) {
	searcher, backend := stageSearcher(t, func(config *SearcherConfig) {
		config.MediaEvidence = NewMediaEvidenceResolver(missingMediaBlobs{})
	})
	backend.hits = []store.ExplainedLexicalCandidate{{
		Node: store.Node{ID: 1, CurrentVersionID: "version-1"}, EvidenceKind: "rendition_segment",
		Locator: document.EvidenceLocatorV1{Kind: document.EvidenceLocatorSegment,
			IndexOrigin: document.EvidenceIndexOriginZero, Start: 0, End: 1000},
	}}
	report, err := searcher.Search(t.Context(), Query{Text: "query", Mode: ModeLexical, Limit: 10})
	require.NoError(t, err)
	require.Equal(t, &MediaTimeSpan{StartMS: 0, EndMS: 1000}, report.Results[0].Evidence[0].TimeSpan)
	require.Zero(t, backend.revalidationCalls)
}

type missingMediaBlobs struct{}

func (missingMediaBlobs) OpenStreamContext(context.Context, string) (packstore.VerifiedReadCloser, int64, error) {
	return nil, 0, os.ErrNotExist
}

func TestMediaEvidenceReusesVerifiedGenerationAcrossLookups(t *testing.T) {
	policy, err := document.NewEvidencePolicy(4096)
	require.NoError(t, err)
	evidence, err := document.NormalizeEvidenceV1(document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1, Family: "audio",
		Completeness: document.EvidenceComplete, UnitKind: document.EvidenceUnitSegment,
		Units: []document.SourceEvidenceUnitV1{
			{Order: 0, Text: "first cue", Locator: document.SourceEvidenceLocatorV1{
				Kind: document.EvidenceLocatorSegment, IndexOrigin: document.EvidenceIndexOriginZero, Start: 0, End: 1000}},
			{Order: 1, Text: "second cue", Locator: document.SourceEvidenceLocatorV1{
				Kind: document.EvidenceLocatorSegment, IndexOrigin: document.EvidenceIndexOriginZero, Start: 7000, End: 9500}},
		},
	}, policy)
	require.NoError(t, err)
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{
		Profile: document.ModelInputProfileCustom, CompatibilityID: "synthetic-space",
		Document: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "document: {{content}}"},
		Query:    document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "query: {{content}}"},
	})
	require.NoError(t, err)
	generation, err := document.BuildEmbeddingInputs(evidence, document.InputPolicy{
		Chunk: document.EmbeddingChunkPolicyV1{ContextFingerprint: strings.Repeat("b", 64),
			Formatter: "evidence-text/v1", MaxTokens: 128, Tokenizer: "synthetic-whole",
			TokenizerRevision: "v1", TruncationPolicy: document.TruncationPolicyReject},
		Tokenizer: mediaTokenizer{}, ModelInput: contract, LexicalEvidenceFingerprint: strings.Repeat("a", 64),
		MaxInputTokens: 256, MaxInputBytes: 4096,
	}, document.GenerationLimits{MaxInputs: 128, MaxTotalContentTokens: 4096, MaxTotalRenderedTokens: 8192,
		MaxTotalContentBytes: 1 << 20, MaxTotalRenderedBytes: 2 << 20,
		MaxFittingWorkTokens: 1 << 20, MaxFittingWorkBytes: 8 << 20})
	require.NoError(t, err)
	generationBytes, err := document.MarshalEmbeddingInputGeneration(generation)
	require.NoError(t, err)
	evidenceBytes, evidenceHash, err := document.MarshalNormalizedEvidenceV1(evidence)
	require.NoError(t, err)
	digest := sha256.Sum256(generationBytes)
	artifacts := store.SearchMediaEvidence{BuildID: "synthetic-build",
		GenerationBlobHash: hex.EncodeToString(digest[:]), GenerationEncodedSize: int64(len(generationBytes)),
		GenerationChecksum: generation.Checksum, EvidenceFingerprint: evidenceHash,
		EvidenceEncodedSize: int64(len(evidenceBytes)), InputCount: len(generation.Inputs)}
	blobs := &mediaBlobFixture{data: map[string][]byte{
		artifacts.GenerationBlobHash: generationBytes, evidenceHash: evidenceBytes,
	}}
	resolver := NewMediaEvidenceResolver(blobs)
	for range 2 {
		for index, want := range []MediaTimeSpan{{StartMS: 0, EndMS: 1000}, {StartMS: 7000, EndMS: 9500}} {
			span, resolveErr := resolver.resolve(t.Context(), artifacts, generation.Inputs[index].Key)
			require.NoError(t, resolveErr)
			require.Equal(t, &want, span)
			span.EndMS = -1 // A caller cannot change a cached interval.
		}
	}
	t.Run("concurrent cached lookups", func(t *testing.T) {
		for range 4 {
			t.Run("lookup", func(t *testing.T) {
				t.Parallel()
				span, resolveErr := resolver.resolve(t.Context(), artifacts, generation.Inputs[0].Key)
				require.NoError(t, resolveErr)
				require.Equal(t, &MediaTimeSpan{StartMS: 0, EndMS: 1000}, span)
			})
		}
	})
	require.Equal(t, 2, blobs.opens, "each immutable artifact is read once across all input lookups")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = resolver.resolve(ctx, artifacts, generation.Inputs[0].Key)
	require.ErrorIs(t, err, context.Canceled)
	changed := artifacts
	changed.GenerationChecksum = strings.Repeat("f", 64)
	_, err = resolver.resolve(t.Context(), changed, generation.Inputs[0].Key)
	require.Error(t, err, "cache entries must match all catalog authority")
}

type mediaTokenizer struct{}

func (mediaTokenizer) Identity() document.TokenizerIdentity {
	return document.TokenizerIdentity{Name: "synthetic-whole", Revision: "v1"}
}
func (mediaTokenizer) PrefixTokenCountsMonotonic() bool { return true }
func (mediaTokenizer) Tokenize(text string, _ int) ([]document.TokenBoundary, error) {
	return []document.TokenBoundary{{Start: 0, End: utf8.RuneCountInString(text)}}, nil
}

type mediaBlobFixture struct {
	data  map[string][]byte
	opens int
}

func (blobs *mediaBlobFixture) OpenStreamContext(_ context.Context, hash string) (packstore.VerifiedReadCloser, int64, error) {
	blobs.opens++
	data, ok := blobs.data[hash]
	if !ok {
		return nil, 0, os.ErrNotExist
	}
	return mediaFixtureStream{io.NopCloser(bytes.NewReader(data))}, int64(len(data)), nil
}

type mediaFixtureStream struct{ io.ReadCloser }

func (mediaFixtureStream) Verify() error  { return nil }
func (mediaFixtureStream) Verified() bool { return true }
