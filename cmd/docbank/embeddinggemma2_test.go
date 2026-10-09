package main

import (
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/openaicompat"
	"go.kenn.io/docbank/internal/config"
)

func TestEmbeddingGemma2TextRuntime(t *testing.T) {
	const variable = "DOCBANK_TEST_EMBEDDINGGEMMA2_KEY"
	t.Setenv(variable, "synthetic-key")
	var calls atomic.Int64
	var response atomic.Pointer[string]
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v1/embeddings", r.URL.Path)
		assert.Equal(t, "Bearer synthetic-key", r.Header.Get("Authorization"))
		var request struct {
			Model          string   `json:"model"`
			Input          []string `json:"input"`
			EncodingFormat string   `json:"encoding_format"`
		}
		// Unknown-member rejection also proves no dimensions or input_type is sent.
		assert.NoError(t, json.UnmarshalRead(r.Body, &request, json.RejectUnknownMembers(true)))
		assert.Equal(t, "embeddinggemma2-text-f32-768-v1", request.Model)
		assert.Equal(t, "float", request.EncodingFormat)
		assert.Equal(t, []string{"title: none | text: synthetic passage", "task: search result | query: synthetic question"}, request.Input)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(*response.Load()))
	}))
	t.Cleanup(server.Close)
	cfg, descriptor := embeddingGemma2Config(t, server.URL, variable)
	bundle, err := configureEmbeddingRuntimeBundle(cfg, unavailableEmbeddingBlobs{}, t.TempDir())
	require.NoError(t, err)
	provider := bundle.providers["semantic"]
	assert.Equal(t, 768, descriptor.Dimension)
	assert.Equal(t, document.VectorNormalizationUnitLength, descriptor.Normalization)
	assert.Equal(t, []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk}, descriptor.InputKinds)
	assert.Equal(t, []document.ModelInputMode{document.ModelInputModeText}, descriptor.SupportedRequestModes)
	inputs := []document.EmbeddingInput{
		{Key: "document", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "synthetic passage"},
		{Key: "query", Role: document.EmbeddingRoleQuery, Kind: document.EmbeddingInputQueryText, Text: "synthetic question"},
	}
	auth := document.EmbeddingAuthorization{ProviderID: descriptor.ID,
		DescriptorFingerprint: descriptor.Fingerprint, PolicyFingerprint: descriptor.PolicyFingerprint,
		MaxBatchItems: 8, MaxInputBytes: 1 << 20, MaxResponseBytes: 1 << 20}
	vector := make([]float32, 768)
	vector[0], vector[1] = 0.6, 0.8
	response.Store(new(embeddingGemma2Response(t, descriptor.Model, vector)))
	result, err := document.ExecuteEmbedding(t.Context(), provider, inputs, auth)
	require.NoError(t, err)
	require.Len(t, result.Vectors, 2)
	for index, item := range result.Vectors {
		assert.Equal(t, inputs[index].Key, item.Key)
		expected := append([]float32(nil), vector...)
		if index == 1 {
			expected[0] = -expected[0]
		}
		assert.Equal(t, expected, item.Values, "the adapter must preserve server-normalized vectors and restore response indices")
	}
	for _, test := range []struct {
		name   string
		values []float32
	}{
		{"wrong width", vector[:767]},
		{"zero norm", make([]float32, 768)},
		{"not normalized", append([]float32{2}, make([]float32, 767)...)},
	} {
		t.Run(test.name, func(t *testing.T) {
			response.Store(new(embeddingGemma2Response(t, descriptor.Model, test.values)))
			result, err := document.ExecuteEmbedding(t.Context(), provider, inputs, auth)
			require.ErrorIs(t, err, openaicompat.ErrMalformedResponse)
			assert.Empty(t, result.Vectors)
		})
	}
	t.Run("nonfinite", func(t *testing.T) {
		response.Store(new(strings.ReplaceAll(embeddingGemma2Response(t, descriptor.Model, vector), "0.6", "1e999")))
		_, err := document.ExecuteEmbedding(t.Context(), provider, inputs, auth)
		require.ErrorIs(t, err, openaicompat.ErrMalformedResponse)
	})
	t.Run("stale authorization", func(t *testing.T) {
		before := calls.Load()
		auth.DescriptorFingerprint = strings.Repeat("0", 64)
		_, err := document.ExecuteEmbedding(t.Context(), provider, inputs, auth)
		require.Error(t, err)
		assert.Equal(t, before, calls.Load(), "authorization must fail before egress")
	})
	for name, mutate := range map[string]func(*config.EmbeddingProfileConfig){
		"query recipe":    func(p *config.EmbeddingProfileConfig) { p.ModelInput.Query.Template += " " },
		"document recipe": func(p *config.EmbeddingProfileConfig) { p.ModelInput.Document.Template += " " },
		"revision": func(p *config.EmbeddingProfileConfig) {
			p.Embedder.FingerprintSalt = "different-revision"
			p.Runtime.ModelRevision, p.Runtime.DeploymentEpoch = "different-revision", "different-revision"
		},
		"dimensions": func(p *config.EmbeddingProfileConfig) { p.Embedder.Dims, p.Dimensions = 512, 512 },
	} {
		t.Run(name+" cannot reuse descriptor", func(t *testing.T) {
			changed := cfg
			p := cfg.EmbeddingProfiles["semantic"]
			p.Runtime, p.Embedder = new(*p.Runtime), new(*p.Embedder)
			mutate(&p)
			changed.EmbeddingProfiles = map[string]config.EmbeddingProfileConfig{"semantic": p}
			_, err := configureEmbeddingRuntimeBundle(changed, unavailableEmbeddingBlobs{}, t.TempDir())
			require.ErrorContains(t, err, "descriptor differs from portable binding")
		})
	}
}

func embeddingGemma2Config(t *testing.T, endpoint, variable string) (config.Config, document.EmbeddingDescriptor) {
	t.Helper()
	fragment, err := os.ReadFile(filepath.Join("testdata", "embeddinggemma2.toml"))
	require.NoError(t, err)
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), fragment, 0o600))
	loaded, err := config.Load(root)
	require.NoError(t, err)
	cfg, _ := syntheticLoopbackOpenAIConfig(t, endpoint, variable)
	p := cfg.EmbeddingProfiles["semantic"]
	recipe := loaded.EmbeddingProfiles["semantic"]
	p.Embedder, p.ModelInput, p.Normalization, p.CompatibilityID = recipe.Embedder, recipe.ModelInput, recipe.Normalization, recipe.CompatibilityID
	p.Embedder.BaseURL = endpoint + "/v1"
	p.Model, p.Dimensions, p.MaxBatchItems = "", 0, 0
	p.Runtime.Endpoint, p.Runtime.ModelRevision, p.Runtime.DeploymentEpoch = "", "", ""
	p.Runtime.RequestTimeout = 0
	cfg.EmbeddingProfiles["semantic"] = p
	p, err = cfg.EmbeddingProfile("semantic")
	require.NoError(t, err)
	contract, err := cfg.EmbeddingModelInput("semantic")
	require.NoError(t, err)
	descriptor, _, err := finalizeOpenAIEmbeddingDescriptor(openaicompat.Profile{
		Origin: p.Runtime.Endpoint, Descriptor: configuredEmbeddingDescriptor(p, contract), ModelInput: contract,
		SecretBinding: p.CredentialBinding, DeploymentEpoch: p.Runtime.DeploymentEpoch,
		RequestTimeout: p.Runtime.RequestTimeout.Std(), MaxBatchItems: p.MaxBatchItems,
		MaxInputBytes: p.MaxInputBytes, MaxRequestBytes: p.Runtime.MaxRequestBytes,
		MaxResponseBytes: p.MaxResponseBytes, EgressPolicy: providerEgressPolicy(p.Runtime.ProviderEgressConfig),
	})
	require.NoError(t, err)
	p.DescriptorFingerprint = descriptor.Fingerprint
	cfg.EmbeddingProfiles["semantic"] = p
	require.NoError(t, cfg.Validate())
	return cfg, descriptor
}

func embeddingGemma2Response(t *testing.T, model string, values []float32) string {
	t.Helper()
	query := append([]float32(nil), values...)
	if len(query) > 0 {
		query[0] = -query[0]
	}
	body, err := json.Marshal(map[string]any{"object": "list", "model": model, "data": []any{
		map[string]any{"object": "embedding", "index": 1, "embedding": query},
		map[string]any{"object": "embedding", "index": 0, "embedding": values},
	}})
	require.NoError(t, err)
	return string(body)
}

func TestEmbeddingGemma2UnsupportedKitSettingsAreRejected(t *testing.T) {
	for _, key := range []string{"document_prefix", "document_suffix", "query_prefix", "query_suffix", "request_dimensions"} {
		t.Run(key, func(t *testing.T) {
			root := t.TempDir()
			value := `"synthetic: "`
			if key == "request_dimensions" {
				value = "true"
			}
			text := "[embedding_profiles.semantic.embedder]\n" + key + " = " + value + "\n"
			require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(text), 0o600))
			_, err := config.Load(root)
			require.ErrorContains(t, err, "unknown key")
			require.ErrorContains(t, err, key)
		})
	}
}

func TestEmbeddingGemma2PreservesLegacyDescriptor(t *testing.T) {
	_, descriptor := syntheticLoopbackOpenAIConfig(t, "http://127.0.0.1:11434", "DOCBANK_TEST_EMBEDDING_KEY")
	assert.Equal(t, "a5676ecd219bb976928cab5c1556f1fd2e2fd4b5604cfa8a8c79953ae92a0500", descriptor.Fingerprint)
}

func TestEmbeddingGemma2RecipeMatchesGuide(t *testing.T) {
	fragment, err := os.ReadFile(filepath.Join("testdata", "embeddinggemma2.toml"))
	require.NoError(t, err)
	guide, err := os.ReadFile(filepath.Join("..", "..", "docs", "configuration.md"))
	require.NoError(t, err)
	guideText := strings.ReplaceAll(string(guide), "\r\n", "\n")
	fragmentText := strings.ReplaceAll(string(fragment), "\r\n", "\n")
	assert.Contains(t, guideText, "```toml\n"+strings.TrimSpace(fragmentText)+"\n```", "the documented fragment must match the exercised recipe")
}
