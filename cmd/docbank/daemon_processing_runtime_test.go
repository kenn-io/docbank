package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/plaintext"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
)

func TestExecutableProcessingProfilesRegistersPlaintextRendition(t *testing.T) {
	provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: plaintext.MaxDocumentBytes})
	require.NoError(t, err)
	descriptor := provider.Descriptor()
	cfg := plaintextProcessingConfig(descriptor.Fingerprint)
	require.NoError(t, cfg.Validate())

	profiles, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
	require.NoError(t, err)
	require.Contains(t, profiles, "private-text")
	require.NotNil(t, profiles["private-text"].RenditionProvider)
	assert.Equal(t, descriptor, profiles["private-text"].RenditionProvider.Descriptor())
	assert.Empty(t, profiles["private-text"].EmbeddingProviders)
}

func TestExecutableProcessingProfilesRejectsDriftedPlaintextDescriptor(t *testing.T) {
	cfg := plaintextProcessingConfig(strings.Repeat("0", 64))
	require.NoError(t, cfg.Validate())

	_, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
	require.ErrorContains(t, err, "descriptor differs from portable binding")
}

func TestExecutableProcessingProfilesRegistersPinnedRenditionChunkTokenizer(t *testing.T) {
	provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: plaintext.MaxDocumentBytes})
	require.NoError(t, err)
	cfg := plaintextProcessingConfig(provider.Descriptor().Fingerprint)
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{
		Profile: document.ModelInputProfileNomic,
	})
	require.NoError(t, err)
	descriptor, err := document.NewEmbeddingDescriptor(document.EmbeddingDescriptor{
		ID: "synthetic.embedding-v1", ContractVersion: document.EmbeddingProviderContractVersion,
		PolicyFingerprint: strings.Repeat("e", 64), TrustBoundary: document.EmbeddingTrustOperatorNetwork,
		Model: "synthetic-model", ModelRevision: "v1", Dimension: 2, Metric: document.VectorMetricCosine,
		Normalization: document.VectorNormalizationNone, ScalarEncoding: "float32",
		DocumentFormatter: "synthetic/document-v1", QueryFormatter: "synthetic/query-v1",
		InputKinds:      []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk},
		CompatibilityID: contract.CompatibilityID, SupportsTextQuery: true, ModelInput: contract,
		SupportedRequestModes: []document.ModelInputMode{contract.Document.Mode},
	})
	require.NoError(t, err)
	cfg.EmbeddingProfiles["semantic"] = config.EmbeddingProfileConfig{
		Activation: string(document.EmbeddingOptional), AuthorizationFingerprint: strings.Repeat("1", 64),
		CompatibilityID: contract.CompatibilityID, CredentialBinding: "credential:semantic",
		DescriptorID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint, Dimensions: descriptor.Dimension,
		DisclosureFingerprint: strings.Repeat("2", 64), DocumentFormatter: descriptor.DocumentFormatter,
		InputKind: string(document.EmbeddingInputRenditionChunk), MaxBatchItems: 8, MaxInputBytes: 1 << 20,
		MaxResponseBytes: 1 << 20, Metric: descriptor.Metric, Model: descriptor.Model,
		Normalization: descriptor.Normalization, QueryFormatter: descriptor.QueryFormatter,
		ScalarEncoding: descriptor.ScalarEncoding, TrustBoundary: string(descriptor.TrustBoundary),
		Chunk: config.EmbeddingChunkConfig{ContextFingerprint: strings.Repeat("3", 64), Formatter: "synthetic/v1",
			MaxTokens: 128, OverlapTokens: 8, Tokenizer: unicodeRuneSpec,
			TruncationPolicy: string(document.TruncationPolicyReject)},
		ModelInput: config.EmbeddingModelInputConfig{Profile: string(document.ModelInputProfileNomic)},
	}
	cfg.ProcessingProfiles["private-text"] = config.ProcessingProfileConfig{
		Rendition: "plaintext", Embeddings: []string{"semantic"}, Retrieval: "lexical",
		AttachmentPolicyFingerprint: strings.Repeat("5", 64), CompletenessFingerprint: strings.Repeat("6", 64),
		ConsentFingerprint: strings.Repeat("7", 64), LexicalSegmenterFingerprint: strings.Repeat("8", 64),
		MaxSegmentRunes: 2000, MaxUnitRunes: 100000, NormalizerFingerprint: strings.Repeat("9", 64),
		RetainSanitizedMarkdown: true, SanitizerFingerprint: strings.Repeat("a", 64), TrustBoundary: "vault-primary",
	}
	require.NoError(t, cfg.Validate())
	bundle := embeddingRuntimeBundle{
		providers: map[string]document.EmbeddingProvider{"semantic": inertEmbeddingProvider{descriptor: descriptor}},
		classifiers: map[string]func(error) (processing.EmbeddingProviderFailure, time.Duration){
			"semantic": func(error) (processing.EmbeddingProviderFailure, time.Duration) {
				return processing.EmbeddingProviderPermanent, 0
			},
		},
	}

	profiles, err := executableProcessingProfiles(cfg, bundle)
	require.NoError(t, err)
	require.Contains(t, profiles, "private-text")
	assert.Equal(t, unicodeRuneSpec,
		profiles["private-text"].Tokenizers["semantic"].Identity().Name+"@"+
			profiles["private-text"].Tokenizers["semantic"].Identity().Revision)
}

type inertEmbeddingProvider struct{ descriptor document.EmbeddingDescriptor }

func (provider inertEmbeddingProvider) Descriptor() document.EmbeddingDescriptor {
	return provider.descriptor
}
func (inertEmbeddingProvider) Embed(context.Context, []document.EmbeddingInput,
	document.EmbeddingAuthorization,
) (document.EmbeddingResult, error) {
	return document.EmbeddingResult{}, nil
}

func plaintextProcessingConfig(descriptorFingerprint string) config.Config {
	cfg := config.Default()
	cfg.RenditionProfiles["plaintext"] = config.RenditionProfileConfig{
		AdapterContract: plaintextRenditionAdapter, AuthorizationFingerprint: strings.Repeat("1", 64),
		CredentialBinding: "credential:none", DeploymentFingerprint: strings.Repeat("2", 64),
		DescriptorID: "plaintext.in-process-v1", DescriptorFingerprint: descriptorFingerprint,
		DisclosureFingerprint: strings.Repeat("3", 64), MaxDocumentBytes: plaintext.MaxDocumentBytes,
		MaxResponseBytes: plaintext.MaxDocumentBytes, MaxUnits: 1,
		RequestedArtifacts: []string{string(document.EvidenceArtifactStructured)},
		TrustBoundary:      string(document.RenditionTrustLocalProcess), UploadOptionsFingerprint: strings.Repeat("4", 64),
	}
	cfg.RetrievalProfiles["lexical"] = config.RetrievalProfileConfig{LexicalLimit: 20, VectorLimit: 20}
	cfg.ProcessingProfiles["private-text"] = config.ProcessingProfileConfig{
		Rendition: "plaintext", Retrieval: "lexical", AttachmentPolicyFingerprint: strings.Repeat("5", 64),
		CompletenessFingerprint: strings.Repeat("6", 64), ConsentFingerprint: strings.Repeat("7", 64),
		LexicalSegmenterFingerprint: strings.Repeat("8", 64), MaxSegmentRunes: 2000, MaxUnitRunes: 100000,
		NormalizerFingerprint: strings.Repeat("9", 64), RetainSanitizedMarkdown: true,
		SanitizerFingerprint: strings.Repeat("a", 64), TrustBoundary: "vault-primary",
	}
	return cfg
}
