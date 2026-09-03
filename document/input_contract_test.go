package document_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

// This test fails if an invalid model-input declaration becomes executable or
// two distinct role envelopes collapse to one durable compatibility identity.
func TestEmbeddingContractCanonicalizesRoleSpecificModelInput(t *testing.T) {
	base, err := document.NewModelInputContract(document.ModelInputContractConfig{Profile: document.ModelInputProfileOpenAICompatible})
	require.NoError(t, err)
	assert.Equal(t, "passage", base.EncodeDocument("passage"))
	assert.Equal(t, "question", base.EncodeQuery("question"))

	for _, testCase := range []struct {
		name   string
		config document.ModelInputContractConfig
		want   string
	}{
		{"unknown built-in", document.ModelInputContractConfig{Profile: "unknown/v1"}, "unknown built-in"},
		{"opaque alias", document.ModelInputContractConfig{Profile: "alias:vendor-default"}, "compatibility"},
		{"missing content slot", document.ModelInputContractConfig{Profile: document.ModelInputProfileCustom, CompatibilityID: "custom-space", Document: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "prefix"}, Query: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "{{content}}"}}, "exactly one content slot"},
		{"two content slots", document.ModelInputContractConfig{Profile: document.ModelInputProfileCustom, CompatibilityID: "custom-space", Document: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "{{content}}{{content}}"}, Query: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "{{content}}"}}, "exactly one content slot"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := document.NewModelInputContract(testCase.config)
			require.ErrorContains(t, err, testCase.want)
		})
	}

	documentEnvelope, err := document.NewModelInputContract(document.ModelInputContractConfig{
		Profile: document.ModelInputProfileCustom, CompatibilityID: "custom-space",
		Document: document.ModelInputEncoder{Mode: document.ModelInputModeDocument, Template: "document: {{content}}"},
		Query:    document.ModelInputEncoder{Mode: document.ModelInputModeQuery, Template: "query: {{content}}"},
	})
	require.NoError(t, err)
	assert.Equal(t, "document: alpha", documentEnvelope.EncodeDocument("alpha"))
	assert.Equal(t, "query: alpha", documentEnvelope.EncodeQuery("alpha"))
	assert.NotEqual(t, base.Fingerprint, documentEnvelope.Fingerprint)
	voyage, err := document.NewModelInputContract(document.ModelInputContractConfig{Profile: document.ModelInputProfileVoyage})
	require.NoError(t, err)
	assert.Equal(t, "passage", voyage.EncodeDocument("passage"))
	assert.Equal(t, "question", voyage.EncodeQuery("question"))
	assert.NotEqual(t, base.Fingerprint, voyage.Fingerprint)
	assert.NotEqual(t, voyage.Document.Mode, voyage.Query.Mode)

	empty, err := document.NewModelInputContract(document.ModelInputContractConfig{})
	require.NoError(t, err)
	assert.NotEqual(t, base.Fingerprint, empty.Fingerprint)
	assert.NotEqual(t, documentEnvelope.Fingerprint, empty.Fingerprint)
}

// This test fails if an adapter's declared native request modes cease to gate
// the model-input contract selected for its descriptor.
func TestEmbeddingContractRejectsUnsupportedProviderRequestMode(t *testing.T) {
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{
		Profile: document.ModelInputProfileCustom, CompatibilityID: "custom-space",
		Document: document.ModelInputEncoder{Mode: document.ModelInputModeDocument, Template: "{{content}}"},
		Query:    document.ModelInputEncoder{Mode: document.ModelInputModeQuery, Template: "{{content}}"},
	})
	require.NoError(t, err)
	_, err = document.NewEmbeddingDescriptor(document.EmbeddingDescriptor{
		ID: "synthetic-embedder", ContractVersion: document.EmbeddingProviderContractVersion,
		PolicyFingerprint: testFingerprint(), TrustBoundary: document.EmbeddingTrustLocalProcess,
		Model: "synthetic-model", ModelRevision: "r1", Dimension: 2, Metric: document.VectorMetricCosine,
		InputKinds: []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk}, CompatibilityID: "custom-space",
		ModelInput: contract, SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeText},
		DocumentFormatter: "document/v1", QueryFormatter: "query/v1", Normalization: document.VectorNormalizationUnitLength,
		ScalarEncoding: "float32",
	})
	require.ErrorContains(t, err, "request mode")
}

func TestEmbeddingContractRequiresOnlyExecutableRequestModes(t *testing.T) {
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{Profile: document.ModelInputProfileCustom, CompatibilityID: "custom-space", Document: document.ModelInputEncoder{Mode: document.ModelInputModeDocument, Template: "{{content}}"}, Query: document.ModelInputEncoder{Mode: document.ModelInputModeQuery, Template: "{{content}}"}})
	require.NoError(t, err)
	descriptor := document.EmbeddingDescriptor{ID: "synthetic-embedder", ContractVersion: document.EmbeddingProviderContractVersion, PolicyFingerprint: testFingerprint(), TrustBoundary: document.EmbeddingTrustLocalProcess, Model: "synthetic-model", ModelRevision: "r1", Dimension: 2, Metric: document.VectorMetricCosine, InputKinds: []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk}, CompatibilityID: "custom-space", ModelInput: contract, SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeDocument}, DocumentFormatter: "document/v1", QueryFormatter: "query/v1", Normalization: document.VectorNormalizationUnitLength, ScalarEncoding: "float32"}
	_, err = document.NewEmbeddingDescriptor(descriptor)
	require.NoError(t, err)
	descriptor.SupportsTextQuery = true
	_, err = document.NewEmbeddingDescriptor(descriptor)
	require.ErrorContains(t, err, "request mode")
}

func TestEmbeddingContractRejectsForgedEmptyModelInput(t *testing.T) {
	empty, err := document.NewModelInputContract(document.ModelInputContractConfig{})
	require.NoError(t, err)
	empty.CompatibilityID = "forged"
	_, err = document.NewEmbeddingDescriptor(document.EmbeddingDescriptor{ID: "synthetic-embedder", ContractVersion: document.EmbeddingProviderContractVersion, PolicyFingerprint: testFingerprint(), TrustBoundary: document.EmbeddingTrustLocalProcess, Model: "synthetic-model", ModelRevision: "r1", Dimension: 2, Metric: document.VectorMetricCosine, InputKinds: []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk}, CompatibilityID: "forged", ModelInput: empty, SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeText}, DocumentFormatter: "document/v1", QueryFormatter: "query/v1", Normalization: document.VectorNormalizationUnitLength, ScalarEncoding: "float32"})
	require.ErrorContains(t, err, "empty model-input")
}
