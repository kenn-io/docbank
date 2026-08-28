package docbank_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	docbank "go.kenn.io/docbank"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/plaintext"
)

func TestRootPackageConstructor(t *testing.T) {
	vault, err := docbank.New(context.Background(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	require.NoError(t, vault.Close())
}

func TestEmbeddedProcessingPlanRunReadAndSearch(t *testing.T) {
	provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
	require.NoError(t, err)
	profile := embeddedProcessingProfile(t, provider.Descriptor())
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(),
		Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{
			"private": {Profile: profile, RenditionProvider: provider},
		}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	_, err = vault.Put(t.Context(), "/needle-outside-fence.txt", strings.NewReader("outside"),
		docbank.PutOptions{MediaType: "text/plain"})
	require.NoError(t, err)
	receipt, err := vault.Put(t.Context(), "/private.txt",
		strings.NewReader("# Private note\n\nA source-fenced needle.\n"),
		docbank.PutOptions{MediaType: "text/plain"})
	require.NoError(t, err)
	selector := docbank.ProcessingSelector{NodeID: receipt.Node.ID,
		ContentVersionID: receipt.Version.ID, Profile: "private"}
	plan, err := vault.PlanProcessing(t.Context(), docbank.ProcessingPlanRequest{Selector: selector})
	require.NoError(t, err)
	require.NotEmpty(t, plan.Fingerprint)
	require.Equal(t, "local_process", plan.Flow[0].TrustBoundary)
	encodedPlan, err := json.Marshal(plan)
	require.NoError(t, err)
	var planWire map[string]any
	require.NoError(t, json.Unmarshal(encodedPlan, &planWire))
	flow, ok := planWire["flow"].([]any)
	require.True(t, ok)
	hop, ok := flow[0].(map[string]any)
	require.True(t, ok)
	runtime, ok := hop["runtime_disclosure"].(map[string]any)
	require.True(t, ok, "embedded plans must expose the same runtime disclosure as HTTP plans")
	require.Equal(t, "in-process", runtime["endpoint"])
	require.Contains(t, plan.RetainedClasses, "sanitized_markdown")
	require.True(t, plan.ConsentRequired)

	job, err := vault.StartProcessing(t.Context(), docbank.StartProcessingRequest{
		PlanRequest:     docbank.ProcessingPlanRequest{Selector: selector},
		PlanFingerprint: plan.Fingerprint, Consent: true,
	})
	require.NoError(t, err)
	status, err := vault.ProcessingStatus(t.Context(), docbank.ProcessingStatusRequest{JobID: job.ID})
	require.NoError(t, err)
	require.Equalf(t, "completed", status.State, "status: %+v", status)
	repeated, err := vault.StartProcessing(t.Context(), docbank.StartProcessingRequest{
		PlanRequest:     docbank.ProcessingPlanRequest{Selector: selector},
		PlanFingerprint: plan.Fingerprint, Consent: true,
	})
	require.NoError(t, err)
	require.Equal(t, job.RenditionJobID, repeated.RenditionJobID)

	rendition, err := vault.Rendition(t.Context(), docbank.RenditionRequest{Selector: selector})
	require.NoError(t, err)
	body, err := io.ReadAll(rendition.Reader)
	require.NoError(t, err)
	require.NoError(t, rendition.Reader.Close())
	require.True(t, bytes.HasPrefix(body, []byte("---\ndocbank:\n  contract: \"docbank-sanitized-markdown/v1\"\n")))
	require.Contains(t, string(body), "    format: \"txt\"")
	require.Contains(t, string(body), "needle")

	fence := docbank.DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{receipt.Version.ID}}
	report, err := vault.SearchDocuments(t.Context(), docbank.DocumentSearchRequest{
		Query: "needle", Mode: docbank.DocumentSearchLexical, Profile: "private", Fence: fence,
	})
	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	require.Equal(t, receipt.Version.ID, report.Results[0].ContentVersionID)
	require.NotContains(t, report.Results[0].Excerpt, "docbank-sanitized-markdown")

	metadataOnly, err := vault.SearchDocuments(t.Context(), docbank.DocumentSearchRequest{
		Query: "sanitized-markdown", Mode: docbank.DocumentSearchLexical, Profile: "private", Fence: fence,
	})
	require.NoError(t, err)
	require.Empty(t, metadataOnly.Results)
}

func TestEmbeddedProcessingRejectsForeignFenceAndClosedVault(t *testing.T) {
	provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
	require.NoError(t, err)
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(),
		Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{
			"private": {Profile: embeddedProcessingProfile(t, provider.Descriptor()), RenditionProvider: provider},
		}}})
	require.NoError(t, err)
	_, err = vault.SearchDocuments(t.Context(), docbank.DocumentSearchRequest{Query: "value",
		Mode: docbank.DocumentSearchLexical, Profile: "private", Fence: docbank.DocumentSourceFence{
			VaultUID:          "00000000-0000-4000-8000-000000000000",
			ContentVersionIDs: []string{"00000000-0000-4000-8000-000000000001"},
		}})
	require.ErrorIs(t, err, docbank.ErrForeignVault)
	require.NoError(t, vault.Close())
	_, err = vault.PlanProcessing(t.Context(), docbank.ProcessingPlanRequest{})
	require.ErrorIs(t, err, docbank.ErrClosed)
}

func TestEmbeddedProcessingRunsDirectEmbeddingsAndSemanticSearch(t *testing.T) {
	renditionProvider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
	require.NoError(t, err)
	embeddingProvider := newSyntheticEmbeddingProvider(t)
	profile := embeddedProcessingProfile(t, renditionProvider.Descriptor())
	profile.Embeddings = []document.EmbeddingBindingV1{syntheticEmbeddingBinding(embeddingProvider.descriptor)}
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(),
		Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{
			"private": {Profile: profile, RenditionProvider: renditionProvider,
				EmbeddingProviders: map[string]document.EmbeddingProvider{"direct": embeddingProvider}},
		}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	receipt, err := vault.Put(t.Context(), "/private.txt", strings.NewReader("semantic needle"),
		docbank.PutOptions{MediaType: "text/plain"})
	require.NoError(t, err)
	selector := docbank.ProcessingSelector{NodeID: receipt.Node.ID,
		ContentVersionID: receipt.Version.ID, Profile: "private"}
	plan, err := vault.PlanProcessing(t.Context(), docbank.ProcessingPlanRequest{Selector: selector})
	require.NoError(t, err)
	job, err := vault.StartProcessing(t.Context(), docbank.StartProcessingRequest{
		PlanRequest: docbank.ProcessingPlanRequest{Selector: selector}, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.NoError(t, err)
	require.Len(t, job.EmbeddingJobIDs, 1)
	require.Equal(t, []string{"document.txt"}, embeddingProvider.filenames)
	aggregate, err := vault.ProcessingStatus(t.Context(), docbank.ProcessingStatusRequest{JobID: job.ID})
	require.NoError(t, err)
	require.Equal(t, "completed", aggregate.State)
	require.Equal(t, 1, aggregate.CompletedBindings)
	require.Equal(t, job.EmbeddingJobIDs, aggregate.EmbeddingJobIDs)
	embeddingStatus, err := vault.ProcessingStatus(t.Context(),
		docbank.ProcessingStatusRequest{JobID: job.EmbeddingJobIDs[0]})
	require.NoError(t, err)
	require.Equal(t, "completed", embeddingStatus.State)

	fence := docbank.DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{receipt.Version.ID}}
	coverage, err := vault.DocumentCoverage(t.Context(), docbank.CoverageRequest{Profile: "private", Fence: fence})
	require.NoError(t, err)
	require.Len(t, coverage.Embeddings, 1)
	require.Equal(t, "complete", coverage.Embeddings[0].State)
	require.Zero(t, coverage.Embeddings[0].Rebuilding)
	require.Zero(t, coverage.Embeddings[0].PreviousGenerationServing)
	report, err := vault.SearchDocuments(t.Context(), docbank.DocumentSearchRequest{
		Query: "needle", Mode: docbank.DocumentSearchSemantic, Profile: "private", BindingID: "direct", Fence: fence})
	require.NoError(t, err)
	require.Equal(t, docbank.DocumentSearchSemantic, report.ActualMode)
	require.Len(t, report.Results, 1)
	require.Equal(t, receipt.Version.ID, report.Results[0].ContentVersionID)
}

func TestEmbeddedProcessingBuildsChunkEmbeddingsFromNormalizedEvidence(t *testing.T) {
	renditionProvider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
	require.NoError(t, err)
	embeddingProvider := newSyntheticEmbeddingProvider(t)
	profile := embeddedProcessingProfile(t, renditionProvider.Descriptor())
	profile.Embeddings = []document.EmbeddingBindingV1{syntheticChunkEmbeddingBinding(embeddingProvider.descriptor)}
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(),
		Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{
			"private": {Profile: profile, RenditionProvider: renditionProvider,
				EmbeddingProviders: map[string]document.EmbeddingProvider{"chunks": embeddingProvider},
				Tokenizers:         map[string]document.Tokenizer{"chunks": syntheticRuneTokenizer{}}},
		}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	receipt, err := vault.Put(t.Context(), "/private.txt", strings.NewReader("chunk semantic needle"),
		docbank.PutOptions{MediaType: "text/plain"})
	require.NoError(t, err)
	selector := docbank.ProcessingSelector{NodeID: receipt.Node.ID,
		ContentVersionID: receipt.Version.ID, Profile: "private"}
	plan, err := vault.PlanProcessing(t.Context(), docbank.ProcessingPlanRequest{Selector: selector})
	require.NoError(t, err)
	_, err = vault.StartProcessing(t.Context(), docbank.StartProcessingRequest{
		PlanRequest: docbank.ProcessingPlanRequest{Selector: selector}, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.NoError(t, err)
	fence := docbank.DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{receipt.Version.ID}}
	report, err := vault.SearchDocuments(t.Context(), docbank.DocumentSearchRequest{
		Query: "needle", Mode: docbank.DocumentSearchSemantic, Profile: "private", BindingID: "chunks", Fence: fence})
	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	require.Equal(t, receipt.Version.ID, report.Results[0].ContentVersionID)
	require.Equal(t, "rendition_chunk", report.Results[0].Evidence[0].InputKind)
}

func TestEmbeddedProcessingSupportsDirectEmbeddingWithoutRenditionProvider(t *testing.T) {
	embeddingProvider := newSyntheticEmbeddingProvider(t)
	profile := embeddedProcessingProfile(t, plaintextDescriptorForProfile(t))
	profile.Rendition = nil
	profile.RetentionDisclosure.RetainSanitizedMarkdown = false
	profile.Embeddings = []document.EmbeddingBindingV1{syntheticEmbeddingBinding(embeddingProvider.descriptor)}
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(),
		Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{
			"direct": {Profile: profile,
				EmbeddingProviders: map[string]document.EmbeddingProvider{"direct": embeddingProvider}},
		}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	receipt, err := vault.Put(t.Context(), "/private.txt", strings.NewReader("direct-only needle"),
		docbank.PutOptions{MediaType: "text/plain"})
	require.NoError(t, err)
	selector := docbank.ProcessingSelector{NodeID: receipt.Node.ID,
		ContentVersionID: receipt.Version.ID, Profile: "direct"}
	plan, err := vault.PlanProcessing(t.Context(), docbank.ProcessingPlanRequest{Selector: selector})
	require.NoError(t, err)
	require.Len(t, plan.Flow, 2)
	require.Equal(t, "embedding", plan.Flow[0].Capability)
	require.Contains(t, plan.DisclosedClasses, "query_text")
	require.NotContains(t, plan.RetainedClasses, "normalized_evidence")
	job, err := vault.StartProcessing(t.Context(), docbank.StartProcessingRequest{
		PlanRequest: docbank.ProcessingPlanRequest{Selector: selector}, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.NoError(t, err)
	require.Empty(t, job.RenditionJobID)
	require.Equal(t, job.EmbeddingJobIDs[0], job.ID)
	status, err := vault.ProcessingStatus(t.Context(), docbank.ProcessingStatusRequest{JobID: job.ID})
	require.NoError(t, err)
	require.Equal(t, "completed", status.State)
	require.Equal(t, 1, status.CompletedBindings)
	fence := docbank.DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{receipt.Version.ID}}
	report, err := vault.SearchDocuments(t.Context(), docbank.DocumentSearchRequest{
		Query: "needle", Mode: docbank.DocumentSearchSemantic, Profile: "direct", BindingID: "direct", Fence: fence})
	require.NoError(t, err)
	require.Len(t, report.Results, 1)
}

func plaintextDescriptorForProfile(t *testing.T) document.RenditionDescriptor {
	t.Helper()
	provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
	require.NoError(t, err)
	return provider.Descriptor()
}

type syntheticEmbeddingProvider struct {
	descriptor document.EmbeddingDescriptor
	filenames  []string
}

func newSyntheticEmbeddingProvider(t *testing.T) *syntheticEmbeddingProvider {
	t.Helper()
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{
		Profile: document.ModelInputProfileCustom, CompatibilityID: "synthetic-direct/v1",
		Document: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "document: {{content}}"},
		Query:    document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "query: {{content}}"},
	})
	require.NoError(t, err)
	descriptor, err := document.NewEmbeddingDescriptor(document.EmbeddingDescriptor{
		ID: "synthetic-direct", ContractVersion: document.EmbeddingProviderContractVersion,
		PolicyFingerprint: embeddedHash("synthetic-embedding-policy"),
		TrustBoundary:     document.EmbeddingTrustLocalProcess, Model: "synthetic", ModelRevision: "v1",
		Dimension: 2, Metric: document.VectorMetricCosine,
		Normalization: document.VectorNormalizationUnitLength, ScalarEncoding: "float32",
		DocumentFormatter: "document/v1", QueryFormatter: "query/v1",
		InputKinds: []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile,
			document.EmbeddingInputRenditionChunk},
		CompatibilityID: contract.CompatibilityID, SupportsTextQuery: true, ModelInput: contract,
		SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeText},
	})
	require.NoError(t, err)
	return &syntheticEmbeddingProvider{descriptor: descriptor}
}

func (provider *syntheticEmbeddingProvider) Descriptor() document.EmbeddingDescriptor {
	return provider.descriptor
}

func (provider *syntheticEmbeddingProvider) Embed(_ context.Context, inputs []document.EmbeddingInput,
	_ document.EmbeddingAuthorization,
) (document.EmbeddingResult, error) {
	vectors := make([]document.EmbeddingVector, len(inputs))
	for index, input := range inputs {
		text := input.Text
		if input.Source != nil {
			provider.filenames = append(provider.filenames, input.Source.Metadata().Filename)
			body, err := io.ReadAll(input.Source)
			if err != nil {
				return document.EmbeddingResult{}, err
			}
			text = string(body)
		}
		vector := []float32{0, 1}
		if strings.Contains(text, "needle") {
			vector = []float32{1, 0}
		}
		vectors[index] = document.EmbeddingVector{Key: input.Key, Values: vector}
	}
	return document.EmbeddingResult{Vectors: vectors}, nil
}

func syntheticEmbeddingBinding(descriptor document.EmbeddingDescriptor) document.EmbeddingBindingV1 {
	return document.EmbeddingBindingV1{Activation: document.EmbeddingRequired,
		AuthorizationFingerprint: embeddedHash("embedding-authorization"),
		CompatibilityID:          descriptor.CompatibilityID, CredentialBinding: "credential:none",
		Descriptor: document.ProviderDescriptorV1{ID: descriptor.ID, Fingerprint: descriptor.Fingerprint},
		Dimensions: descriptor.Dimension, DisclosureFingerprint: embeddedHash("embedding-disclosure"),
		DocumentFormatter: descriptor.DocumentFormatter, InputKind: document.EmbeddingInputOriginalFile,
		MaxBatchItems: 8, MaxInputBytes: 1 << 20, MaxResponseBytes: 1 << 20,
		Metric: descriptor.Metric, ModelInput: descriptor.ModelInput, Model: descriptor.Model, Name: "direct",
		Normalization: descriptor.Normalization, QueryFormatter: descriptor.QueryFormatter,
		ScalarEncoding: descriptor.ScalarEncoding, TrustBoundary: string(descriptor.TrustBoundary)}
}

func syntheticChunkEmbeddingBinding(descriptor document.EmbeddingDescriptor) document.EmbeddingBindingV1 {
	binding := syntheticEmbeddingBinding(descriptor)
	binding.Name = "chunks"
	binding.InputKind = document.EmbeddingInputRenditionChunk
	binding.Chunk = &document.EmbeddingChunkPolicyV1{ContextFingerprint: embeddedHash("chunk-context"),
		Formatter: "rendition-chunk/v1", MaxTokens: 64, OverlapTokens: 8,
		Tokenizer: "synthetic-runes@v1", TruncationPolicy: string(document.TruncationPolicyReject)}
	return binding
}

type syntheticRuneTokenizer struct{}

func (syntheticRuneTokenizer) Identity() document.TokenizerIdentity {
	return document.TokenizerIdentity{Name: "synthetic-runes", Revision: "v1",
		PrefixTokenCountsMonotonic: true}
}

func (syntheticRuneTokenizer) Tokenize(text string, limit int) ([]document.TokenBoundary, error) {
	runes := []rune(text)
	if len(runes) > limit {
		return nil, document.ErrTokenizerLimit
	}
	result := make([]document.TokenBoundary, len(runes))
	for index := range runes {
		result[index] = document.TokenBoundary{Start: index, End: index + 1}
	}
	return result, nil
}

func embeddedHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func embeddedProcessingProfile(t *testing.T, descriptor document.RenditionDescriptor) document.ProcessingProfileV1 {
	t.Helper()
	hash := func(value string) string {
		digest := sha256.Sum256([]byte(value))
		return hex.EncodeToString(digest[:])
	}
	return document.ProcessingProfileV1{
		ContractVersion: document.ProcessingProfileContractV1,
		Rendition: &document.RenditionBindingV1{
			AdapterContract: "plaintext.in-process/v1", AuthorizationFingerprint: hash("authorization"),
			CredentialBinding: "credential:none", DeploymentFingerprint: hash("deployment"),
			Descriptor:            document.ProviderDescriptorV1{ID: descriptor.ID, Fingerprint: descriptor.Fingerprint},
			DisclosureFingerprint: hash("rendition-disclosure"), MaxDocumentBytes: 1 << 20,
			MaxResponseBytes: 1 << 20, MaxUnits: 1000, Name: "plaintext",
			RequestedArtifacts: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
			TrustBoundary:      string(descriptor.TrustBoundary), UploadOptionsFingerprint: hash("upload"),
		},
		EvidenceLexical: document.EvidenceLexicalPolicyV1{
			CompletenessFingerprint: hash("completeness"), LexicalSegmenterFingerprint: hash("segments"),
			MaxSegmentRunes: 1000, MaxUnitRunes: 100_000,
			NormalizedEvidenceContract: document.NormalizedEvidenceContractV1,
			NormalizerFingerprint:      hash("normalizer"), RenditionContract: document.RenditionContractV1,
			SanitizerFingerprint: hash("sanitizer"), SourceEvidenceContract: document.SourceEvidenceContractV1,
		},
		RetentionDisclosure: document.RetentionDisclosurePolicyV1{
			AttachmentPolicyFingerprint: hash("attachment"), ConsentFingerprint: hash("consent"),
			RetainSanitizedMarkdown: true, TrustBoundary: string(descriptor.TrustBoundary),
		},
		Retrieval: document.RetrievalPolicyV1{LexicalLimit: 50, VectorLimit: 50},
	}
}

func createRangeFixture(
	t *testing.T, vault *docbank.Vault, virtualPath string, content []byte,
) docbank.PutReceipt {
	t.Helper()
	sum := sha256.Sum256(content)
	receipt, err := vault.Create(
		t.Context(), virtualPath, bytes.NewReader(content),
		docbank.CreateOptions{
			MediaType: "application/octet-stream",
			Expected: docbank.ContentIdentity{
				SHA256: hex.EncodeToString(sum[:]),
				Size:   int64(len(content)),
			},
		},
	)
	require.NoError(t, err)
	return receipt
}

func TestOpenVersionContentRangeRejectsInvalidSlices(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	receipt := createRangeFixture(
		t, vault, "/ranges/value.bin", []byte("0123456789"),
	)

	cases := []docbank.ContentRangeOptions{
		{Offset: -1, Length: 1},
		{Offset: 0, Length: 0},
		{Offset: 0, Length: -1},
		{Offset: 10, Length: 1},
		{Offset: 9, Length: 2},
		{Offset: math.MaxInt64, Length: math.MaxInt64},
	}
	for _, opts := range cases {
		_, err := vault.OpenVersionContentRange(t.Context(), receipt.Version.ID, opts)
		require.ErrorIs(t, err, docbank.ErrInvalidContentRange)
	}
}

func TestOpenVersionContentRangeMissingVersion(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })

	_, err = vault.OpenVersionContentRange(
		t.Context(), "00000000-0000-4000-8000-000000000000",
		docbank.ContentRangeOptions{Offset: 0, Length: 1},
	)
	require.ErrorIs(t, err, docbank.ErrNotFound)
}

func TestOpenVersionContentRangeRawLoose(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	receipt := createRangeFixture(
		t, vault, "/ranges/raw.bin", []byte("0123456789"),
	)

	got, err := vault.OpenVersionContentRange(
		t.Context(), receipt.Version.ID,
		docbank.ContentRangeOptions{Offset: 2, Length: 4},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, got.Reader.Close()) })
	body, err := io.ReadAll(got.Reader)
	require.NoError(t, err)
	require.Equal(t, []byte("2345"), body)
	require.Equal(t, receipt.Version, got.Version)
	require.Equal(t, int64(2), got.Offset)
	require.Equal(t, int64(4), got.Length)
}

func TestOpenVersionContentRangeHistoricalVersion(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	path := "/ranges/history.bin"
	first := createRangeFixture(t, vault, path, []byte("abcdefghij"))
	_, err = vault.Put(
		t.Context(), path, bytes.NewReader([]byte("ABCDEFGHIJ")),
		docbank.PutOptions{MediaType: "application/octet-stream"},
	)
	require.NoError(t, err)

	got, err := vault.OpenVersionContentRange(
		t.Context(), first.Version.ID,
		docbank.ContentRangeOptions{Offset: 1, Length: 3},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, got.Reader.Close()) })
	body, err := io.ReadAll(got.Reader)
	require.NoError(t, err)
	require.Equal(t, []byte("bcd"), body)
	require.Equal(t, first.Version.ID, got.Version.ID)
}

func TestOpenVersionContentRangeCompressedLoose(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{
		Root: t.TempDir(),
		LooseCompression: docbank.LooseCompressionOptions{
			Enabled: true, MinBytes: 1, MinSavingsPercent: 0,
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	content := []byte(strings.Repeat("compressed logical range\n", 128))
	receipt := createRangeFixture(t, vault, "/ranges/compressed.bin", content)
	require.Equal(t, "loose", receipt.Physical.Kind)
	require.Equal(t, "zstd", receipt.Physical.Encoding)

	got, err := vault.OpenVersionContentRange(
		t.Context(), receipt.Version.ID,
		docbank.ContentRangeOptions{Offset: 11, Length: 17},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, got.Reader.Close()) })
	body, err := io.ReadAll(got.Reader)
	require.NoError(t, err)
	require.Equal(t, content[11:28], body)
}

func TestOpenVersionContentRangePacked(t *testing.T) {
	root := t.TempDir()
	vault, err := docbank.New(t.Context(), docbank.Config{Root: root})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	content := []byte("packed logical range content")
	receipt := createRangeFixture(t, vault, "/ranges/packed.bin", content)
	report, err := vault.Pack(t.Context(), docbank.PackOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, report.BlobsPacked)
	require.NoFileExists(t, looseBlobPath(root, receipt.Computed.SHA256))

	got, err := vault.OpenVersionContentRange(
		t.Context(), receipt.Version.ID,
		docbank.ContentRangeOptions{Offset: 7, Length: 7},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, got.Reader.Close()) })
	body, err := io.ReadAll(got.Reader)
	require.NoError(t, err)
	require.Equal(t, content[7:14], body)
}

func TestOpenVersionContentRangeUnavailable(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(*testing.T, string)
	}{
		{
			name: "missing authority",
			corrupt: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.Remove(path))
			},
		},
		{
			name: "physical size mismatch",
			corrupt: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.WriteFile(path, []byte("short"), 0o600))
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			vault, err := docbank.New(t.Context(), docbank.Config{Root: root})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, vault.Close()) })
			content := []byte("physical authority bytes")
			receipt := createRangeFixture(t, vault, "/ranges/unavailable.bin", content)
			test.corrupt(t, looseBlobPath(root, receipt.Computed.SHA256))

			_, err = vault.OpenVersionContentRange(
				t.Context(), receipt.Version.ID,
				docbank.ContentRangeOptions{Offset: 0, Length: 1},
			)
			require.ErrorIs(t, err, docbank.ErrContentUnavailable)
		})
	}
}

func TestOpenVersionContentRangeHoldsVaultLease(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	receipt := createRangeFixture(t, vault, "/ranges/lease.bin", []byte("lease bytes"))
	opened, err := vault.OpenVersionContentRange(
		t.Context(), receipt.Version.ID,
		docbank.ContentRangeOptions{Offset: 0, Length: 1},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, opened.Reader.Close()) })

	closeDone := make(chan error, 1)
	go func() { closeDone <- vault.Close() }()
	select {
	case err := <-closeDone:
		require.FailNow(t, "vault closed while a range held its lifecycle lease", "error: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	require.NoError(t, opened.Reader.Close())
	select {
	case err := <-closeDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "vault did not close after the range released its lease")
	}
}

func TestOpenVersionContentRangeClosedVault(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	receipt := createRangeFixture(t, vault, "/ranges/closed.bin", []byte("closed bytes"))
	require.NoError(t, vault.Close())

	_, err = vault.OpenVersionContentRange(
		t.Context(), receipt.Version.ID,
		docbank.ContentRangeOptions{Offset: 0, Length: 1},
	)
	require.ErrorIs(t, err, docbank.ErrClosed)
}

func TestEmbeddedImmutableCreate(t *testing.T) {
	content := []byte("immutable external content\n")
	sum := sha256.Sum256(content)
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })

	receipt, err := vault.Create(t.Context(), "/external.txt", bytes.NewReader(content), docbank.CreateOptions{
		MediaType: "text/plain",
		Expected:  docbank.ContentIdentity{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(content))},
	})
	require.NoError(t, err)
	require.True(t, receipt.Created)
}

func TestVaultMoveTrashRestoreExternalAPI(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })

	created, err := vault.Put(
		t.Context(), "/inbox/report.txt", strings.NewReader("report\n"), docbank.PutOptions{},
	)
	require.NoError(t, err)

	moved, err := vault.MovePath(t.Context(), "/inbox/report.txt", "/archive.txt", docbank.RevisionOptions{
		IfRevision: created.Node.Revision,
	})
	require.NoError(t, err)
	require.Equal(t, created.Node.ID, moved.Node.ID)
	require.Equal(t, created.Node.Revision+1, moved.Node.Revision)
	require.Equal(t, "/archive.txt", moved.Path)

	trashed, err := vault.TrashPath(t.Context(), moved.Path, docbank.RevisionOptions{
		IfRevision: moved.Node.Revision,
	})
	require.NoError(t, err)
	require.Equal(t, moved.Path, trashed.Path)
	restored, err := vault.Restore(t.Context(), trashed.Node.ID, docbank.RevisionOptions{
		IfRevision: trashed.Node.Revision,
	})
	require.NoError(t, err)
	require.Equal(t, moved.Path, restored.Path)

	_, err = vault.TrashPath(t.Context(), restored.Path, docbank.RevisionOptions{})
	require.NoError(t, err)
	report, err := vault.EmptyTrash(t.Context(), docbank.TrashEmptyOptions{MaxRoots: 1, DryRun: true})
	require.NoError(t, err)
	require.Equal(t, int64(1), report.Candidates)
	require.True(t, report.DryRun)
}

func TestVaultMoveBatchExternalAPI(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	first, err := vault.Put(t.Context(), "/left/first.txt", strings.NewReader("first\n"), docbank.PutOptions{})
	require.NoError(t, err)
	second, err := vault.Put(t.Context(), "/right/second.txt", strings.NewReader("second\n"), docbank.PutOptions{})
	require.NoError(t, err)

	receipts, err := vault.BatchMove(t.Context(), []docbank.BatchMoveItem{
		{SourcePath: "/left/first.txt", DestinationPath: "/right/second.txt"},
		{NodeID: second.Node.ID, IfRevision: second.Node.Revision, DestinationPath: "/left/first.txt"},
	})
	require.NoError(t, err)
	require.Len(t, receipts, 2)
	require.Equal(t, first.Node.ID, receipts[0].Node.ID)
	require.Equal(t, "/left/first.txt", receipts[0].FromPath)
	require.Equal(t, "/right/second.txt", receipts[0].Path)
	require.Equal(t, second.Node.ID, receipts[1].Node.ID)
	require.Equal(t, "/left/first.txt", receipts[1].Path)
}

func TestTreeMutationErrorsAreClassifiableOutsidePackage(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })

	created, err := vault.Put(
		t.Context(), "/parent/child/document.txt", strings.NewReader("document\n"),
		docbank.PutOptions{},
	)
	require.NoError(t, err)

	_, err = vault.Restore(t.Context(), created.Node.ID, docbank.RevisionOptions{})
	require.ErrorIs(t, err, docbank.ErrNotTrashed)
	_, err = vault.TrashPath(t.Context(), "/", docbank.RevisionOptions{})
	require.ErrorIs(t, err, docbank.ErrIsRoot)
	_, err = vault.MovePath(
		t.Context(), "/parent/child/document.txt", "/parent/../document.txt",
		docbank.RevisionOptions{},
	)
	require.ErrorIs(t, err, docbank.ErrInvalidName)
	_, err = vault.MovePath(
		t.Context(), "/parent", "/parent/child/parent", docbank.RevisionOptions{},
	)
	require.ErrorIs(t, err, docbank.ErrCycle)

	// Existing audited vaults can surface this sentinel through the same public
	// methods even though first enrollment is currently daemon-owned.
	require.ErrorIs(t, fmt.Errorf("embedded audited mutation: %w", docbank.ErrAuditMutationUnsupported), docbank.ErrAuditMutationUnsupported)
}

func TestOpenContentClassifiesPhysicalContentFailures(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(*testing.T, string)
	}{
		{
			name: "missing blob",
			corrupt: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.Remove(path))
			},
		},
		{
			name: "physical size mismatch",
			corrupt: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.WriteFile(path, []byte("short"), 0o600))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			vault, err := docbank.New(t.Context(), docbank.Config{Root: root})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, vault.Close()) })

			receipt, err := vault.Put(
				t.Context(), "/notes/current.md", strings.NewReader("current bytes\n"), docbank.PutOptions{},
			)
			require.NoError(t, err)
			test.corrupt(t, looseBlobPath(root, receipt.Computed.SHA256))

			_, err = vault.OpenContent(t.Context(), "/notes/current.md")
			require.ErrorIs(t, err, docbank.ErrContentUnavailable)
		})
	}
}

func TestOpenVersionContentClassifiesPhysicalContentFailures(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(*testing.T, string)
	}{
		{
			name: "missing blob",
			corrupt: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.Remove(path))
			},
		},
		{
			name: "physical size mismatch",
			corrupt: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.WriteFile(path, []byte("short"), 0o600))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			vault, err := docbank.New(t.Context(), docbank.Config{Root: root})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, vault.Close()) })

			first, err := vault.Put(
				t.Context(), "/notes/history.md", strings.NewReader("historical bytes\n"), docbank.PutOptions{},
			)
			require.NoError(t, err)
			_, err = vault.Put(
				t.Context(), "/notes/history.md", strings.NewReader("current bytes\n"), docbank.PutOptions{},
			)
			require.NoError(t, err)
			test.corrupt(t, looseBlobPath(root, first.Computed.SHA256))

			_, err = vault.OpenVersionContent(t.Context(), first.Version.ID)
			require.ErrorIs(t, err, docbank.ErrContentUnavailable)
		})
	}
}

func TestContentMetadataErrorsRemainDistinctFromPhysicalUnavailability(t *testing.T) {
	root := t.TempDir()
	vault, err := docbank.New(t.Context(), docbank.Config{Root: root})
	require.NoError(t, err)

	receipt, err := vault.Put(
		t.Context(), "/notes/entry.md", strings.NewReader("entry\n"), docbank.PutOptions{},
	)
	require.NoError(t, err)

	_, err = vault.OpenContent(t.Context(), "/missing.md")
	require.ErrorIs(t, err, docbank.ErrNotFound)
	require.NotErrorIs(t, err, docbank.ErrContentUnavailable)

	_, err = vault.OpenContent(t.Context(), "/notes")
	require.ErrorIs(t, err, docbank.ErrNotFile)
	require.NotErrorIs(t, err, docbank.ErrContentUnavailable)

	_, err = vault.OpenVersionContent(t.Context(), "00000000-0000-4000-8000-000000000000")
	require.ErrorIs(t, err, docbank.ErrNotFound)
	require.NotErrorIs(t, err, docbank.ErrContentUnavailable)

	require.NoError(t, vault.Close())

	_, err = vault.OpenContent(t.Context(), "/notes/entry.md")
	require.ErrorIs(t, err, docbank.ErrClosed)
	require.NotErrorIs(t, err, docbank.ErrContentUnavailable)

	_, err = vault.OpenVersionContent(t.Context(), receipt.Version.ID)
	require.ErrorIs(t, err, docbank.ErrClosed)
	require.NotErrorIs(t, err, docbank.ErrContentUnavailable)
}

func looseBlobPath(root, hash string) string {
	return filepath.Join(root, "blobs", hash[:2], hash)
}
