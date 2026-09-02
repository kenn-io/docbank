package document_test

import (
	"bytes"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestCanonicalProfileMatchesGoldenAndCanonicalizesInput(t *testing.T) {
	profile := syntheticProcessingProfileV1()
	profile.Rendition.Descriptor.ID = "mistral-Cafe\u0301\r\nruntime"
	profile.Embeddings[0].Model = "voyage-Cafe\u0301\r\nmodel"
	original := cloneProcessingProfile(profile)

	encoded, fingerprints, err := document.CanonicalProfile(profile)
	require.NoError(t, err)
	want, err := os.ReadFile("testdata/profile-v1.golden.json")
	require.NoError(t, err)
	want = bytes.TrimSuffix(want, []byte("\n"))
	assert.Equal(t, want, encoded)
	assert.Equal(t, original, profile, "canonicalization must not mutate caller-owned policy")
	assert.Equal(t, "d1f4c031ff9161c9034d3fb56c871a91423d84564fc48024e80a9ec4e45057ff", fingerprints.Profile)
	assert.Equal(t, "9d0a202be29b43e16684f74b540a41778443fe2f4757d4b1815d85994f5b2522", fingerprints.RenditionRequest)
	assert.Equal(t, "1a79d280a3eefc9b8e6402e6f9a491783db7c2804ae00ac03160a963a48aa7d7", fingerprints.EvidenceLexical)
	assert.Equal(t, map[string]string{
		"direct":   "59c57d1c5b5b3106ce6310660b0ec8aa038c58c20bdcaad9ef711c40b0ae9ff1",
		"semantic": "d39106ffbbf86c37f9c51d229535e7bde503cf12fffcd25560544bb8fa128f3b",
	}, fingerprints.EmbeddingInput)
	assert.Equal(t, map[string]string{
		"direct":   "dc2da50bb2d163a114834bbcee2ea439cd838da23f4e0ac8a504d9b007487357",
		"semantic": "9bb1db44ac603c26f0f4028b009d50e2ca83cef5b61fa4fd2dad4108b31a541b",
	}, fingerprints.VectorSpace)
	assert.Equal(t, "3debf7c7ff983edfebee3fe195f5db2a2d32dae3ff3f64eb834e382925e2a1ef", fingerprints.RetentionDisclosure)

	all := flattenFingerprints(fingerprints)
	for _, fingerprint := range all {
		assert.Regexp(t, `^[0-9a-f]{64}$`, fingerprint)
	}
	assert.Len(t, uniqueStrings(all), len(all), "the sample's semantic layers must have independent identities")

	composed := cloneProcessingProfile(profile)
	composed.Rendition.Descriptor.ID = "mistral-Café\nruntime"
	composed.Embeddings[0].Model = "voyage-Café\nmodel"
	composedJSON, composedFingerprints, err := document.CanonicalProfile(composed)
	require.NoError(t, err)
	assert.JSONEq(t, string(encoded), string(composedJSON), "NFC and LF-equivalent policy must have one identity")
	assert.Equal(t, fingerprints, composedFingerprints)

	reordered := cloneProcessingProfile(profile)
	slices.Reverse(reordered.Embeddings)
	slices.Reverse(reordered.Rendition.RequestedArtifacts)
	reorderedJSON, reorderedFingerprints, err := document.CanonicalProfile(reordered)
	require.NoError(t, err)
	assert.JSONEq(t, string(encoded), string(reorderedJSON))
	assert.Equal(t, fingerprints, reorderedFingerprints)

	empty := cloneProcessingProfile(profile)
	empty.Embeddings = []document.EmbeddingBindingV1{}
	nilEmbeddings := cloneProcessingProfile(empty)
	nilEmbeddings.Embeddings = nil
	emptyJSON, emptyFingerprints, err := document.CanonicalProfile(empty)
	require.NoError(t, err)
	nilJSON, nilFingerprints, err := document.CanonicalProfile(nilEmbeddings)
	require.NoError(t, err)
	assert.JSONEq(t, string(emptyJSON), string(nilJSON))
	assert.Equal(t, emptyFingerprints, nilFingerprints)
}

func TestCanonicalProfileFingerprintsTrackExactLayerFields(t *testing.T) {
	base := syntheticProcessingProfileV1()
	_, baseline, err := document.CanonicalProfile(base)
	require.NoError(t, err)

	tests := []struct {
		name   string
		mutate func(*document.ProcessingProfileV1)
		want   []string
	}{
		{"rendition adapter contract", func(p *document.ProcessingProfileV1) { p.Rendition.AdapterContract = "adapter/v2" }, layers("profile", "rendition")},
		{"rendition authorization", func(p *document.ProcessingProfileV1) { p.Rendition.AuthorizationFingerprint = fingerprint("c") }, layers("profile")},
		{"rendition credential reference", func(p *document.ProcessingProfileV1) { p.Rendition.CredentialBinding = "credential:ocr-secondary" }, layers("profile")},
		{"rendition deployment", func(p *document.ProcessingProfileV1) { p.Rendition.DeploymentFingerprint = fingerprint("c") }, layers("profile", "rendition", "retention")},
		{"rendition descriptor id", func(p *document.ProcessingProfileV1) { p.Rendition.Descriptor.ID = "mistral-ocr-v2" }, layers("profile", "rendition", "retention")},
		{"rendition descriptor fingerprint", func(p *document.ProcessingProfileV1) { p.Rendition.Descriptor.Fingerprint = fingerprint("c") }, layers("profile", "rendition", "retention")},
		{"filename disclosure", func(p *document.ProcessingProfileV1) { p.Rendition.DiscloseFilename = true }, layers("profile", "rendition", "retention")},
		{"rendition disclosure policy", func(p *document.ProcessingProfileV1) { p.Rendition.DisclosureFingerprint = fingerprint("c") }, layers("profile", "rendition", "retention")},
		{"rendition document limit", func(p *document.ProcessingProfileV1) { p.Rendition.MaxDocumentBytes++ }, layers("profile", "rendition")},
		{"rendition response limit", func(p *document.ProcessingProfileV1) { p.Rendition.MaxResponseBytes++ }, layers("profile", "rendition")},
		{"rendition unit limit", func(p *document.ProcessingProfileV1) { p.Rendition.MaxUnits++ }, layers("profile", "rendition")},
		{"rendition binding name", func(p *document.ProcessingProfileV1) { p.Rendition.Name = "secondary" }, layers("profile")},
		{"requested artifact roles", func(p *document.ProcessingProfileV1) {
			p.Rendition.RequestedArtifacts = append(p.Rendition.RequestedArtifacts, document.EvidenceArtifactImage)
		}, layers("profile", "rendition")},
		{"rendition trust boundary", func(p *document.ProcessingProfileV1) { p.Rendition.TrustBoundary = "processor-secondary" }, layers("profile", "retention")},
		{"rendition upload options", func(p *document.ProcessingProfileV1) { p.Rendition.UploadOptionsFingerprint = fingerprint("c") }, layers("profile", "rendition")},
		{"normalizer", func(p *document.ProcessingProfileV1) { p.EvidenceLexical.NormalizerFingerprint = fingerprint("c") }, layers("profile", "evidence", "input:semantic")},
		{"sanitizer", func(p *document.ProcessingProfileV1) { p.EvidenceLexical.SanitizerFingerprint = fingerprint("c") }, layers("profile", "evidence", "input:semantic")},
		{"completeness rules", func(p *document.ProcessingProfileV1) { p.EvidenceLexical.CompletenessFingerprint = fingerprint("c") }, layers("profile", "evidence", "input:semantic")},
		{"lexical segmenter", func(p *document.ProcessingProfileV1) {
			p.EvidenceLexical.LexicalSegmenterFingerprint = fingerprint("c")
		}, layers("profile", "evidence", "input:semantic")},
		{"document character limit", func(p *document.ProcessingProfileV1) {
			p.EvidenceLexical.MaxDocumentChars++
		}, layers("profile", "evidence", "input:semantic")},
		{"unit rune limit", func(p *document.ProcessingProfileV1) { p.EvidenceLexical.MaxUnitRunes++ }, layers("profile", "evidence", "input:semantic")},
		{"segment rune limit", func(p *document.ProcessingProfileV1) { p.EvidenceLexical.MaxSegmentRunes++ }, layers("profile", "evidence", "input:semantic")},
		{"retrieval lexical limit", func(p *document.ProcessingProfileV1) { p.Retrieval.LexicalLimit++ }, layers("profile")},
		{"retrieval vector limit", func(p *document.ProcessingProfileV1) { p.Retrieval.VectorLimit++ }, layers("profile")},
		{"embedding activation", func(p *document.ProcessingProfileV1) { p.Embeddings[0].Activation = document.EmbeddingRequired }, layers("profile")},
		{"embedding authorization", func(p *document.ProcessingProfileV1) { p.Embeddings[0].AuthorizationFingerprint = fingerprint("c") }, layers("profile")},
		{"embedding batch limit", func(p *document.ProcessingProfileV1) { p.Embeddings[0].MaxBatchItems++ }, layers("profile")},
		{"embedding byte limit", func(p *document.ProcessingProfileV1) { p.Embeddings[0].MaxInputBytes++ }, layers("profile")},
		{"embedding response limit", func(p *document.ProcessingProfileV1) { p.Embeddings[0].MaxResponseBytes++ }, layers("profile")},
		{"embedding credential reference", func(p *document.ProcessingProfileV1) {
			p.Embeddings[0].CredentialBinding = "credential:embedding-secondary"
		}, layers("profile")},
		{"embedding descriptor id", func(p *document.ProcessingProfileV1) { p.Embeddings[0].Descriptor.ID = "voyage-multimodal-v2" }, layers("profile", "vector:direct", "retention")},
		{"embedding descriptor fingerprint", func(p *document.ProcessingProfileV1) { p.Embeddings[0].Descriptor.Fingerprint = fingerprint("c") }, layers("profile", "vector:direct", "retention")},
		{"embedding disclosure policy", func(p *document.ProcessingProfileV1) { p.Embeddings[0].DisclosureFingerprint = fingerprint("c") }, layers("profile", "retention")},
		{"embedding document formatter", func(p *document.ProcessingProfileV1) { p.Embeddings[0].DocumentFormatter = "direct-file/v2" }, layers("profile", "vector:direct")},
		{"embedding compatibility id", func(p *document.ProcessingProfileV1) { p.Embeddings[0].CompatibilityID = "voyage-multimodal-v2" }, layers("profile", "vector:direct")},
		{"embedding dimensions", func(p *document.ProcessingProfileV1) { p.Embeddings[0].Dimensions++ }, layers("profile", "vector:direct")},
		{"embedding metric", func(p *document.ProcessingProfileV1) { p.Embeddings[0].Metric = "dot_product" }, layers("profile", "vector:direct")},
		{"embedding model", func(p *document.ProcessingProfileV1) { p.Embeddings[0].Model = "voyage-multimodal-3.5" }, layers("profile", "vector:direct")},
		{"embedding normalization", func(p *document.ProcessingProfileV1) { p.Embeddings[0].Normalization = "none" }, layers("profile", "vector:direct")},
		{"embedding query formatter", func(p *document.ProcessingProfileV1) { p.Embeddings[0].QueryFormatter = "query/v2" }, layers("profile", "vector:direct")},
		{"embedding scalar encoding", func(p *document.ProcessingProfileV1) { p.Embeddings[0].ScalarEncoding = "float16" }, layers("profile", "vector:direct")},
		{"embedding trust boundary", func(p *document.ProcessingProfileV1) { p.Embeddings[0].TrustBoundary = "processor-secondary" }, layers("profile", "retention")},
		{"embedding binding name", func(p *document.ProcessingProfileV1) { p.Embeddings[0].Name = "direct-secondary" },
			layers("profile", "input:direct", "input:direct-secondary", "vector:direct", "vector:direct-secondary")},
		{"embedding input kind", func(p *document.ProcessingProfileV1) {
			p.Embeddings[0].InputKind = document.EmbeddingInputRenditionChunk
			chunk := *p.Embeddings[1].Chunk
			p.Embeddings[0].Chunk = &chunk
		}, layers("profile", "input:direct", "retention")},
		{"chunk tokenizer", func(p *document.ProcessingProfileV1) { p.Embeddings[1].Chunk.Tokenizer = "voyage-4" }, layers("profile", "input:semantic")},
		{"chunk maximum", func(p *document.ProcessingProfileV1) { p.Embeddings[1].Chunk.MaxTokens++ }, layers("profile", "input:semantic")},
		{"chunk overlap", func(p *document.ProcessingProfileV1) { p.Embeddings[1].Chunk.OverlapTokens++ }, layers("profile", "input:semantic")},
		{"chunk truncation", func(p *document.ProcessingProfileV1) { p.Embeddings[1].Chunk.TruncationPolicy = "truncate_end" }, layers("profile", "input:semantic")},
		{"chunk formatter", func(p *document.ProcessingProfileV1) { p.Embeddings[1].Chunk.Formatter = "rendition-chunk/v2" }, layers("profile", "input:semantic")},
		{"chunk context", func(p *document.ProcessingProfileV1) { p.Embeddings[1].Chunk.ContextFingerprint = fingerprint("c") }, layers("profile", "input:semantic")},
		{"attachment policy", func(p *document.ProcessingProfileV1) {
			p.RetentionDisclosure.AttachmentPolicyFingerprint = fingerprint("c")
		}, layers("profile", "retention")},
		{"consent", func(p *document.ProcessingProfileV1) { p.RetentionDisclosure.ConsentFingerprint = fingerprint("c") }, layers("profile", "retention")},
		{"provider markdown retention", func(p *document.ProcessingProfileV1) { p.RetentionDisclosure.RetainProviderMarkdown = true }, layers("profile", "retention")},
		{"sanitized markdown retention", func(p *document.ProcessingProfileV1) { p.RetentionDisclosure.RetainSanitizedMarkdown = false }, layers("profile", "retention")},
		{"typed artifact retention", func(p *document.ProcessingProfileV1) { p.RetentionDisclosure.RetainTypedArtifacts = false }, layers("profile", "retention")},
		{"attachment trust boundary", func(p *document.ProcessingProfileV1) { p.RetentionDisclosure.TrustBoundary = "vault-secondary" }, layers("profile", "retention")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := cloneProcessingProfile(base)
			test.mutate(&changed)
			_, got, err := document.CanonicalProfile(changed)
			require.NoError(t, err)
			assert.ElementsMatch(t, test.want, changedFingerprintLayers(baseline, got))
		})
	}
}

func TestCanonicalProfileRejectsInvalidPolicy(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*document.ProcessingProfileV1)
		want   string
	}{
		{"duplicate binding name", func(p *document.ProcessingProfileV1) { p.Embeddings[1].Name = "direct" }, "duplicated"},
		{"unknown input kind", func(p *document.ProcessingProfileV1) { p.Embeddings[0].InputKind = "summary" }, "input kind"},
		{"chunk embedding without rendition", func(p *document.ProcessingProfileV1) {
			p.Rendition = nil
			p.RetentionDisclosure.RetainSanitizedMarkdown = false
		}, "rendition_chunk"},
		{"retained markdown without rendition", func(p *document.ProcessingProfileV1) { p.Rendition = nil; p.Embeddings = p.Embeddings[:1] }, "retained Markdown"},
		{"invalid UTF-8", func(p *document.ProcessingProfileV1) { p.Embeddings[0].Model = string([]byte{0xff}) }, "UTF-8"},
		{"zero lexical retrieval limit", func(p *document.ProcessingProfileV1) { p.Retrieval.LexicalLimit = 0 }, "retrieval lexical limit"},
		{"excessive vector retrieval limit", func(p *document.ProcessingProfileV1) { p.Retrieval.VectorLimit = 1_001 }, "retrieval vector limit"},
		{"unsupported source evidence contract", func(p *document.ProcessingProfileV1) {
			p.EvidenceLexical.SourceEvidenceContract = "source-evidence/v2"
		}, "source evidence contract"},
		{"unsupported normalized evidence contract", func(p *document.ProcessingProfileV1) {
			p.EvidenceLexical.NormalizedEvidenceContract = "normalized-evidence/v2"
		}, "normalized evidence contract"},
		{"unsupported rendition contract", func(p *document.ProcessingProfileV1) {
			p.EvidenceLexical.RenditionContract = "rendition/v2"
		}, "rendition contract"},
		{"raw rendition secret", func(p *document.ProcessingProfileV1) { p.Rendition.CredentialBinding = "sk-test-do-not-persist" }, "credential:"},
		{"raw embedding secret", func(p *document.ProcessingProfileV1) { p.Embeddings[0].CredentialBinding = "sk-test-do-not-persist" }, "credential:"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			profile := cloneProcessingProfile(syntheticProcessingProfileV1())
			test.mutate(&profile)
			_, _, err := document.CanonicalProfile(profile)
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestCanonicalProfileEnforcesPortableBounds(t *testing.T) {
	tests := []struct {
		name string
		max  int64
		set  func(*document.ProcessingProfileV1, int64)
		want string
	}{
		{"rendition document bytes", 1 << 40, func(p *document.ProcessingProfileV1, v int64) { p.Rendition.MaxDocumentBytes = v }, "max document bytes"},
		{"rendition response bytes", 1 << 30, func(p *document.ProcessingProfileV1, v int64) { p.Rendition.MaxResponseBytes = v }, "max response bytes"},
		{"rendition units", 1_000_000, func(p *document.ProcessingProfileV1, v int64) { p.Rendition.MaxUnits = int(v) }, "max units"},
		{"evidence unit runes", 256 << 20, func(p *document.ProcessingProfileV1, v int64) { p.EvidenceLexical.MaxUnitRunes = int(v) }, "max unit runes"},
		{"evidence segment runes", 1 << 20, func(p *document.ProcessingProfileV1, v int64) { p.EvidenceLexical.MaxSegmentRunes = int(v) }, "max segment runes"},
		{"embedding dimensions", 1_000_000, func(p *document.ProcessingProfileV1, v int64) { p.Embeddings[0].Dimensions = int(v) }, "dimensions"},
		{"embedding batch items", 10_000, func(p *document.ProcessingProfileV1, v int64) { p.Embeddings[0].MaxBatchItems = int(v) }, "max batch items"},
		{"embedding input bytes", 1 << 30, func(p *document.ProcessingProfileV1, v int64) { p.Embeddings[0].MaxInputBytes = v }, "max input bytes"},
		{"embedding response bytes", 1 << 30, func(p *document.ProcessingProfileV1, v int64) { p.Embeddings[0].MaxResponseBytes = v }, "max response bytes"},
		{"chunk tokens", 1_000_000, func(p *document.ProcessingProfileV1, v int64) { p.Embeddings[1].Chunk.MaxTokens = int(v) }, "max tokens"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			atMaximum := cloneProcessingProfile(syntheticProcessingProfileV1())
			test.set(&atMaximum, test.max)
			if test.name == "evidence segment runes" {
				atMaximum.EvidenceLexical.MaxUnitRunes = int(test.max)
			}
			_, _, err := document.CanonicalProfile(atMaximum)
			require.NoError(t, err)
			aboveMaximum := cloneProcessingProfile(atMaximum)
			test.set(&aboveMaximum, test.max+1)
			if test.name == "evidence segment runes" {
				aboveMaximum.EvidenceLexical.MaxUnitRunes = int(test.max + 1)
			}
			_, _, err = document.CanonicalProfile(aboveMaximum)
			require.ErrorContains(t, err, test.want)
		})
	}

	atMaximum := cloneProcessingProfile(syntheticProcessingProfileV1())
	for index := len(atMaximum.Embeddings); index < 64; index++ {
		binding := atMaximum.Embeddings[0]
		binding.Name = fmt.Sprintf("binding-%02d", index)
		atMaximum.Embeddings = append(atMaximum.Embeddings, binding)
	}
	_, _, err := document.CanonicalProfile(atMaximum)
	require.NoError(t, err)
	aboveMaximum := cloneProcessingProfile(atMaximum)
	extra := aboveMaximum.Embeddings[0]
	extra.Name = "binding-overflow"
	aboveMaximum.Embeddings = append(aboveMaximum.Embeddings, extra)
	_, _, err = document.CanonicalProfile(aboveMaximum)
	require.ErrorContains(t, err, "too many embedding bindings")

	atMaximum = cloneProcessingProfile(syntheticProcessingProfileV1())
	atMaximum.Rendition.RequestedArtifacts = []document.EvidenceArtifactRole{document.EvidenceArtifactImage, document.EvidenceArtifactMarkdown, document.EvidenceArtifactStructured, document.EvidenceArtifactTranscript}
	_, _, err = document.CanonicalProfile(atMaximum)
	require.NoError(t, err)
	aboveMaximum = cloneProcessingProfile(atMaximum)
	aboveMaximum.Rendition.RequestedArtifacts = append(aboveMaximum.Rendition.RequestedArtifacts, document.EvidenceArtifactImage)
	_, _, err = document.CanonicalProfile(aboveMaximum)
	require.ErrorContains(t, err, "too many requested artifacts")
}

func syntheticProcessingProfileV1() document.ProcessingProfileV1 {
	return document.ProcessingProfileV1{
		ContractVersion: document.ProcessingProfileContractV1,
		Rendition: &document.RenditionBindingV1{
			AdapterContract: "mistral-ocr-adapter/v1", AuthorizationFingerprint: fingerprint("2"), CredentialBinding: "credential:ocr-primary",
			DeploymentFingerprint: fingerprint("d"), Descriptor: document.ProviderDescriptorV1{ID: "mistral-ocr-v1", Fingerprint: fingerprint("1")},
			DisclosureFingerprint: fingerprint("e"), MaxDocumentBytes: 10 << 20, MaxResponseBytes: 4 << 20, MaxUnits: 256, Name: "primary",
			RequestedArtifacts: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured, document.EvidenceArtifactMarkdown},
			TrustBoundary:      "processor-primary", UploadOptionsFingerprint: fingerprint("f"),
		},
		EvidenceLexical: document.EvidenceLexicalPolicyV1{
			CompletenessFingerprint: fingerprint("6"), LexicalSegmenterFingerprint: fingerprint("5"), MaxDocumentChars: 100_000,
			MaxSegmentRunes: 2_000, MaxUnitRunes: 100_000,
			NormalizedEvidenceContract: document.NormalizedEvidenceContractV1, NormalizerFingerprint: fingerprint("3"),
			RenditionContract: document.RenditionContractV1, SanitizerFingerprint: fingerprint("4"), SourceEvidenceContract: document.SourceEvidenceContractV1,
		},
		Retrieval: document.RetrievalPolicyV1{LexicalLimit: 40, VectorLimit: 60},
		Embeddings: []document.EmbeddingBindingV1{
			{Activation: document.EmbeddingOptional, AuthorizationFingerprint: fingerprint("7"), CompatibilityID: "voyage-multimodal-3/1024",
				CredentialBinding: "credential:embedding-primary", Descriptor: document.ProviderDescriptorV1{ID: "voyage-multimodal-v1", Fingerprint: fingerprint("8")},
				Dimensions: 1024, DisclosureFingerprint: fingerprint("9"), DocumentFormatter: "direct-file/v1", InputKind: document.EmbeddingInputOriginalFile,
				MaxBatchItems: 8, MaxInputBytes: 8 << 20, MaxResponseBytes: 1 << 20, Metric: "cosine", Model: "voyage-multimodal-3", Name: "direct",
				Normalization: "unit_length", QueryFormatter: "query/v1", ScalarEncoding: "float32", TrustBoundary: "processor-primary"},
			{Activation: document.EmbeddingRequired, AuthorizationFingerprint: fingerprint("a"),
				Chunk: &document.EmbeddingChunkPolicyV1{ContextFingerprint: fingerprint("b"), Formatter: "rendition-chunk/v1", MaxTokens: 800,
					OverlapTokens: 80, Tokenizer: "voyage-3", TruncationPolicy: "reject"},
				CompatibilityID: "voyage-3-large/1024", CredentialBinding: "credential:embedding-primary",
				Descriptor: document.ProviderDescriptorV1{ID: "voyage-text-v1", Fingerprint: fingerprint("c")}, Dimensions: 1024,
				DisclosureFingerprint: fingerprint("d"), DocumentFormatter: "document/v1", InputKind: document.EmbeddingInputRenditionChunk,
				MaxBatchItems: 32, MaxInputBytes: 1 << 20, MaxResponseBytes: 1 << 20, Metric: "cosine", Model: "voyage-3-large", Name: "semantic",
				Normalization: "unit_length", QueryFormatter: "query/v1", ScalarEncoding: "float32", TrustBoundary: "processor-primary"},
		},
		RetentionDisclosure: document.RetentionDisclosurePolicyV1{AttachmentPolicyFingerprint: fingerprint("a"), ConsentFingerprint: fingerprint("b"),
			RetainSanitizedMarkdown: true, RetainTypedArtifacts: true, TrustBoundary: "vault-primary"},
	}
}

func cloneProcessingProfile(profile document.ProcessingProfileV1) document.ProcessingProfileV1 {
	clone := profile
	if profile.Rendition != nil {
		rendition := *profile.Rendition
		rendition.RequestedArtifacts = slices.Clone(profile.Rendition.RequestedArtifacts)
		clone.Rendition = &rendition
	}
	clone.Embeddings = slices.Clone(profile.Embeddings)
	for index := range clone.Embeddings {
		if profile.Embeddings[index].Chunk != nil {
			chunk := *profile.Embeddings[index].Chunk
			clone.Embeddings[index].Chunk = &chunk
		}
	}
	return clone
}

func changedFingerprintLayers(before, after document.FingerprintSet) []string {
	left, right := flattenedFingerprintSet(before), flattenedFingerprintSet(after)
	keys := make(map[string]struct{}, len(left)+len(right))
	for key := range left {
		keys[key] = struct{}{}
	}
	for key := range right {
		keys[key] = struct{}{}
	}
	changed := make([]string, 0)
	for key := range keys {
		if left[key] != right[key] {
			changed = append(changed, key)
		}
	}
	return changed
}

func flattenedFingerprintSet(set document.FingerprintSet) map[string]string {
	result := map[string]string{"profile": set.Profile, "rendition": set.RenditionRequest, "evidence": set.EvidenceLexical, "retention": set.RetentionDisclosure}
	for name, value := range set.EmbeddingInput {
		result["input:"+name] = value
	}
	for name, value := range set.VectorSpace {
		result["vector:"+name] = value
	}
	return result
}

func flattenFingerprints(set document.FingerprintSet) []string {
	values := make([]string, 0, 4+len(set.EmbeddingInput)+len(set.VectorSpace))
	for _, value := range flattenedFingerprintSet(set) {
		values = append(values, value)
	}
	return values
}

func uniqueStrings(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}
func fingerprint(char string) string   { return strings.Repeat(char, 64) }
func layers(values ...string) []string { return values }
