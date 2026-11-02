package config

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/openaicompat"
	"go.kenn.io/docbank/document/plaintext"
)

const LocalTextProfile = "local-text"

// LocalTextConfig selects vault-wide, ongoing text indexing after approval.
type LocalTextConfig struct {
	Version int    `json:"version"`
	Profile string `json:"profile"`
	Server  string `json:"server"`
}

func localTextFingerprint(value string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte("docbank/local-text/v1/"+value)))
}

// WithLocalText supplies the built-in Nomic fallback. Runtime discovery
// remains lazy and performs no network access here.
func (c Config) WithLocalText() (Config, error) {
	if _, exists := c.ProcessingProfiles[LocalTextProfile]; exists {
		return c, fmt.Errorf("processing profile %q is reserved for local text", LocalTextProfile)
	}
	provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: plaintext.MaxDocumentBytes})
	if err != nil {
		return c, err
	}
	descriptor := provider.Descriptor()
	h := localTextFingerprint
	c.RenditionProfiles[LocalTextProfile] = RenditionProfileConfig{ //nolint:gosec // Stable anonymous binding identities contain no credentials.
		AdapterContract: "docbank-plaintext-rendition/v1", AuthorizationFingerprint: h("render-authorization"),
		CredentialBinding: "credential:local-text", DeploymentFingerprint: h("render-deployment"),
		DescriptorID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
		DisclosureFingerprint: h("render-disclosure"), MaxDocumentBytes: plaintext.MaxDocumentBytes,
		MaxResponseBytes: 64 << 20, MaxUnits: 1, RequestedArtifacts: []string{"structured_evidence"},
		TrustBoundary: string(descriptor.TrustBoundary), UploadOptionsFingerprint: h("render-upload"),
	}
	c.EmbeddingProfiles[LocalTextProfile] = EmbeddingProfileConfig{ //nolint:gosec // Stable anonymous binding identities contain no credentials.
		Activation: "optional", AuthorizationFingerprint: h("embed-authorization"), CompatibilityID: "nomic/search/v1",
		CredentialBinding: "credential:local-text", DescriptorID: openaicompat.ProviderID,
		Dimensions: 768, DisclosureFingerprint: h("embed-disclosure"), DocumentFormatter: openaicompat.DocumentFormatterV1,
		QueryFormatter: openaicompat.QueryFormatterV1, InputKind: "rendition_chunk", MaxBatchItems: 32,
		MaxInputBytes: 1 << 20, MaxInputTokens: 1024, MaxResponseBytes: 32 << 20,
		Metric: "cosine", Model: "nomic-embed-text", Normalization: "unit_length", ScalarEncoding: "float32",
		TrustBoundary: string(document.EmbeddingTrustOperatorNetwork), ModelInput: EmbeddingModelInputConfig{Profile: string(document.ModelInputProfileNomic)},
		Chunk: EmbeddingChunkConfig{ //nolint:gosec // This fingerprint identifies chunk policy.
			ContextFingerprint: h("chunk-context"), Formatter: "chunk/v1", MaxTokens: 1024,
			OverlapTokens: 128, Tokenizer: "unicode-runes", TokenizerRevision: "v1", TruncationPolicy: "reject_indivisible"},
		Runtime: &EmbeddingRuntimeConfig{Managed: true, AdapterContract: "docbank-openai-compatible-embeddings/v1",
			ModelRevision: "nomic-embed-text-v1.5", DeploymentEpoch: "nomic-embed-text-v1.5",
			RequestTimeout: Duration(30 * time.Second), MaxRequestBytes: 2 << 20},
	}
	c.RetrievalProfiles[LocalTextProfile] = RetrievalProfileConfig{LexicalLimit: 100, VectorLimit: 100}
	c.ProcessingProfiles[LocalTextProfile] = ProcessingProfileConfig{
		Rendition: LocalTextProfile, Embeddings: []string{LocalTextProfile}, Retrieval: LocalTextProfile,
		AttachmentPolicyFingerprint: h("attachments"), CompletenessFingerprint: h("completeness"),
		ConsentFingerprint: h("consent"), LexicalSegmenterFingerprint: h("segmenter"),
		MaxDocumentChars: 16 << 20, MaxSegmentRunes: 4096, MaxUnitRunes: 16 << 20,
		NormalizerFingerprint: h("normalizer"), SanitizerFingerprint: h("sanitizer"),
		RetainSanitizedMarkdown: true, TrustBoundary: "vault",
	}
	return c, nil
}

// ManagedEmbedding identifies the daemon-owned Kit provider override.
func ManagedEmbedding(p EmbeddingProfileConfig) bool {
	return p.Runtime != nil && p.Runtime.AdapterContract == "docbank-openai-compatible-embeddings/v1" &&
		p.Runtime.Managed
}

// ManagedEmbeddingProfile derives the same stable provider policy at validation
// and runtime construction. Discovery is injected only by the runtime owner.
func ManagedEmbeddingProfile(p EmbeddingProfileConfig, resolve func(context.Context) (string, error)) (openaicompat.Profile, error) {
	if p.Runtime == nil || p.Model != "nomic-embed-text" || p.Dimensions != 768 {
		return openaicompat.Profile{}, errors.New("managed embeddings require nomic-embed-text with 768 dimensions")
	}
	input, err := embeddingModelInputContract(p.ModelInput)
	if err != nil {
		return openaicompat.Profile{}, err
	}
	mode := p.Runtime.ServerMode
	if mode == "" {
		mode = "auto"
	}
	if resolve == nil {
		resolve = func(context.Context) (string, error) {
			return "", errors.New("local embeddings runtime is unavailable")
		}
	}
	d, err := document.NewEmbeddingDescriptor(EmbeddingDescriptor(p, input))
	if err != nil {
		return openaicompat.Profile{}, err
	}
	profile := openaicompat.Profile{ManagedService: "kit-embed", SelectionMode: mode, ResolveOrigin: resolve,
		Descriptor: d, ModelInput: input, DeploymentEpoch: p.Runtime.DeploymentEpoch,
		RequestTimeout: p.Runtime.RequestTimeout.Std(), MaxBatchItems: p.MaxBatchItems, MaxInputBytes: p.MaxInputBytes,
		MaxRequestBytes: p.Runtime.MaxRequestBytes, MaxResponseBytes: p.MaxResponseBytes,
	}
	fingerprint, err := openaicompat.PolicyFingerprint(profile)
	if err != nil {
		return profile, err
	}
	d.PolicyFingerprint, d.Fingerprint = fingerprint, ""
	profile.Descriptor, err = document.NewEmbeddingDescriptor(d)
	return profile, err
}

func EmbeddingDescriptor(profile EmbeddingProfileConfig, modelInput document.ModelInputContract) document.EmbeddingDescriptor {
	runtime := profile.Runtime
	modes := []document.ModelInputMode{modelInput.Document.Mode}
	supportsQuery := runtime.AdapterContract == "docbank-standard-embedding-bridge/v2" ||
		runtime.AdapterContract == "docbank-openai-compatible-embeddings/v1" ||
		(runtime.AdapterContract == "docbank-voyage-embeddings/v1" && profile.InputKind == string(document.EmbeddingInputRenditionChunk))
	if supportsQuery && modelInput.Query.Mode != modelInput.Document.Mode {
		modes = append(modes, modelInput.Query.Mode)
	}
	return document.EmbeddingDescriptor{ID: profile.DescriptorID,
		ContractVersion: document.EmbeddingProviderContractVersion, PolicyFingerprint: strings.Repeat("0", 64),
		TrustBoundary: document.EmbeddingTrustBoundary(profile.TrustBoundary), Model: profile.Model,
		ModelRevision: runtime.ModelRevision, Dimension: profile.Dimensions, Metric: profile.Metric,
		Normalization: profile.Normalization, ScalarEncoding: profile.ScalarEncoding,
		DocumentFormatter: profile.DocumentFormatter, QueryFormatter: profile.QueryFormatter,
		InputKinds:      []document.EmbeddingInputKind{document.EmbeddingInputKind(profile.InputKind)},
		CompatibilityID: profile.CompatibilityID, SupportsTextQuery: supportsQuery,
		ModelInput: modelInput, SupportedRequestModes: modes}
}
