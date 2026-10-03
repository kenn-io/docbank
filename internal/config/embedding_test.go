package config

import (
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/secretref"
)

func TestKitEmbedderPreservesPortableProfile(t *testing.T) {
	t.Parallel()
	cfg := validProcessingConfig()
	profile := cfg.EmbeddingProfiles["semantic"]
	profile.ModelInput = EmbeddingModelInputConfig{Profile: string(document.ModelInputProfileNomic)}
	profile.CompatibilityID = "nomic/search/v1"
	profile.Runtime = &EmbeddingRuntimeConfig{
		AdapterContract: "docbank-openai-compatible-embeddings/v1", Endpoint: "https://embedding.example.invalid:443",
		ModelRevision: "deployment-v1", DeploymentEpoch: "deployment-v1",
		RequestTimeout: Duration(time.Second), MaxRequestBytes: 1 << 20,
		AllowedCIDRs: []string{"192.0.2.0/24"}, ProxyMode: "disabled",
		ConnectTimeout: Duration(time.Second), KeepAlive: Duration(time.Second), TLSHandshakeTimeout: Duration(time.Second),
	}
	cfg.CredentialBindings = map[string]CredentialBindingConfig{
		"embedding-primary": {EnvironmentVariable: "DOCBANK_TEST_KIT_KEY"},
	}
	cfg.EmbeddingProfiles["semantic"] = profile
	want, err := cfg.ProcessingProfile("archive")
	require.NoError(t, err)
	wantJSON, err := json.Marshal(want.Document, json.Deterministic(true))
	require.NoError(t, err)
	profile.Embedder = &embedconfig.Embedder{
		BaseURL: profile.Runtime.Endpoint + "/v1", Model: profile.Model, Dims: profile.Dimensions,
		FingerprintSalt: profile.Runtime.ModelRevision, BatchSize: profile.MaxBatchItems, TimeoutSeconds: 1,
		APIKey: secretref.Ref{Env: "DOCBANK_TEST_KIT_KEY"},
	}
	profile.Model, profile.Dimensions, profile.MaxBatchItems = "", 0, 0
	profile.Runtime.Endpoint, profile.Runtime.ModelRevision, profile.Runtime.DeploymentEpoch = "", "", ""
	profile.Runtime.RequestTimeout = 0
	cfg.EmbeddingProfiles["semantic"] = profile
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(`
[embedding_profiles.semantic.embedder]
base_url = "https://embedding.example.invalid:443/v1"
model = "model"
dims = 8
fingerprint_salt = "deployment-v1"
batch_size = 8
timeout_seconds = 1
api_key = { env = "DOCBANK_TEST_KIT_KEY" }
`), 0o600))
	loaded, err := Load(root)
	require.NoError(t, err)
	profile.Embedder = loaded.EmbeddingProfiles["semantic"].Embedder
	cfg.EmbeddingProfiles["semantic"] = profile
	loaded = cfg
	require.NoError(t, loaded.Validate())
	got, err := loaded.ProcessingProfile("archive")
	require.NoError(t, err)
	gotJSON, err := json.Marshal(got.Document, json.Deterministic(true))
	require.NoError(t, err)
	assert.JSONEq(t, string(wantJSON), string(gotJSON))
	assert.NotContains(t, string(gotJSON), "DOCBANK_TEST_KIT_KEY")
	resolved, err := loaded.EmbeddingProfile("semantic")
	require.NoError(t, err)
	assert.Equal(t, "https://embedding.example.invalid:443", resolved.Runtime.Endpoint)
	assert.Empty(t, loaded.EmbeddingProfiles["semantic"].Model, "resolution must not mutate the source configuration")

	for name, mutate := range map[string]func(*EmbeddingProfileConfig){
		"model conflict":            func(p *EmbeddingProfileConfig) { p.Model = "different-model" },
		"dimensions conflict":       func(p *EmbeddingProfileConfig) { p.Dimensions = 42 },
		"batch conflict":            func(p *EmbeddingProfileConfig) { p.MaxBatchItems = 2 },
		"endpoint conflict":         func(p *EmbeddingProfileConfig) { p.Runtime.Endpoint = "https://other.example.invalid" },
		"revision conflict":         func(p *EmbeddingProfileConfig) { p.Runtime.ModelRevision = "different-weights" },
		"timeout conflict":          func(p *EmbeddingProfileConfig) { p.Runtime.RequestTimeout = Duration(2 * time.Second) },
		"credential conflict":       func(p *EmbeddingProfileConfig) { p.Embedder.APIKey = secretref.Literal("synthetic-key") },
		"unsupported role":          func(p *EmbeddingProfileConfig) { p.Embedder.InputTypeMode = "retrieval" },
		"unsupported token packing": func(p *EmbeddingProfileConfig) { p.Embedder.ModelContextTokens, p.Embedder.MaxBatchTokens = 100, 1000 },
		"unsupported route":         func(p *EmbeddingProfileConfig) { p.Embedder.BaseURL = "https://embedding.example.invalid/other" },
		"native provider":           func(p *EmbeddingProfileConfig) { p.Runtime.AdapterContract = "docbank-voyage-embeddings/v1" },
	} {
		t.Run(name, func(t *testing.T) {
			p := profile
			p.Runtime, p.Embedder = new(*profile.Runtime), new(*profile.Embedder)
			mutate(&p)
			c := cfg
			c.EmbeddingProfiles = map[string]EmbeddingProfileConfig{"semantic": p}
			if name == "native provider" {
				_, err := c.EmbeddingProfile("semantic")
				require.ErrorContains(t, err, "embedder is only supported for OpenAI-compatible text runtimes")
				return
			}
			require.Error(t, c.Validate())
		})
	}

	t.Run("shared credential conflicts", func(t *testing.T) {
		c := cfg
		c.CredentialBindings = nil
		other := profile
		other.Embedder = new(*profile.Embedder)
		other.Embedder.APIKey = secretref.Literal("different-synthetic-key")
		c.EmbeddingProfiles = map[string]EmbeddingProfileConfig{"semantic": profile, "other": other}
		require.ErrorContains(t, c.Validate(), "same credential binding")
	})

	// A real model change remains incompatible with the existing portable
	// binding; the configuration syntax must not erase it.
	profile.Embedder.Model = "different-model"
	cfg.EmbeddingProfiles["semantic"] = profile
	changed, err := cfg.ProcessingProfile("archive")
	require.NoError(t, err)
	changedJSON, err := json.Marshal(changed.Document, json.Deterministic(true))
	require.NoError(t, err)
	assert.NotEqual(t, string(wantJSON), string(changedJSON))
	assert.Contains(t, string(changedJSON), "different-model")
}
