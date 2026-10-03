package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/openaicompat"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/safefileio"
	"go.kenn.io/kit/secretref"
)

func TestKitEmbeddingConfigCredentialsPreserveDescriptorAndRequests(t *testing.T) {
	for _, source := range []string{"inline", "env", "file"} {
		t.Run(source, func(t *testing.T) {
			const variable = "DOCBANK_TEST_KIT_EMBEDDING_KEY"
			t.Setenv(variable, "")
			var calls atomic.Int64
			var expected atomic.Pointer[string]
			expected.Store(new("synthetic-first"))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, "Bearer "+*expected.Load(), r.Header.Get("Authorization"))
				assert.Equal(t, "/v1/embeddings", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"object":"list","model":"synthetic-model","data":[{"object":"embedding","index":0,"embedding":[2,0]}]}`))
			}))
			t.Cleanup(server.Close)
			cfg, descriptor := syntheticLoopbackOpenAIConfig(t, server.URL, variable)
			root := t.TempDir()
			keyFile := filepath.Join(root, "embedding.key")
			key := strconv.Quote("synthetic-first")
			switch source {
			case "env":
				key = "{ env = " + strconv.Quote(variable) + " }"
			case "file":
				key = "{ file = " + strconv.Quote(keyFile) + " }"
			}
			text := fmt.Sprintf(`[embedding_profiles.semantic.embedder]
base_url = %q
model = "synthetic-model"
dims = 2
fingerprint_salt = "deployment-v1"
batch_size = 8
timeout_seconds = 1
api_key = %s
`, server.URL+"/v1", key)
			require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(text), 0o600))
			loaded, err := config.Load(root)
			require.NoError(t, err)
			p := cfg.EmbeddingProfiles["semantic"]
			p.Embedder = loaded.EmbeddingProfiles["semantic"].Embedder
			p.Model, p.Dimensions, p.MaxBatchItems = "", 0, 0
			p.Runtime.Endpoint, p.Runtime.ModelRevision, p.Runtime.DeploymentEpoch = "", "", ""
			p.Runtime.RequestTimeout = 0
			if source == "inline" {
				p.Runtime.Endpoint = server.URL + "/"
			}
			cfg.EmbeddingProfiles["semantic"] = p
			cfg.CredentialBindings = nil
			require.NoError(t, cfg.Validate())
			bundle, err := configureEmbeddingRuntimeBundle(cfg, unavailableEmbeddingBlobs{}, t.TempDir())
			require.NoError(t, err, "startup must not resolve file or environment secrets")
			provider := bundle.providers["semantic"]
			assert.Equal(t, descriptor, provider.Descriptor())
			inputs := []document.EmbeddingInput{{Key: "document-1", Role: document.EmbeddingRoleDocument,
				Kind: document.EmbeddingInputRenditionChunk, Text: "passage"}}
			auth := document.EmbeddingAuthorization{ProviderID: descriptor.ID,
				DescriptorFingerprint: descriptor.Fingerprint, PolicyFingerprint: descriptor.PolicyFingerprint,
				MaxBatchItems: 8, MaxInputBytes: 1 << 20, MaxResponseBytes: 1 << 20}
			if source != "inline" {
				_, err = provider.Embed(t.Context(), inputs, auth)
				require.ErrorIs(t, err, openaicompat.ErrUnauthorized)
				assert.Zero(t, calls.Load())
			}
			if source == "file" {
				// Mode 0600 alone does not establish a protected Windows DACL.
				file, err := safefileio.CreatePrivateFile(keyFile)
				require.NoError(t, err)
				require.NoError(t, file.Close())
			}
			for _, value := range []string{"synthetic-first", "synthetic-rotated"} {
				if source == "inline" && value != "synthetic-first" {
					break
				}
				expected.Store(new(value))
				switch source {
				case "env":
					t.Setenv(variable, value)
				case "file":
					require.NoError(t, os.WriteFile(keyFile, []byte(value+"\n"), 0o600))
				}
				result, err := provider.Embed(t.Context(), inputs, auth)
				require.NoError(t, err)
				assert.Equal(t, []float32{2, 0}, result.Vectors[0].Values, "Kit schema must not enable vector normalization")
			}
			assert.Equal(t, descriptor, provider.Descriptor(), "secret rotation must not change vector identity")
			// A genuine model or revision change must still reject the old pinned descriptor.
			for _, change := range []string{"model", "revision"} {
				changed := p
				changed.Embedder = new(*p.Embedder)
				if change == "model" {
					changed.Embedder.Model = "different-model"
				} else {
					changed.Embedder.FingerprintSalt = "different-weights"
				}
				cfg.EmbeddingProfiles["semantic"] = changed
				_, err = configureEmbeddingRuntimeBundle(cfg, unavailableEmbeddingBlobs{}, t.TempDir())
				require.ErrorContains(t, err, "descriptor differs from portable binding")
			}
		})
	}
}

func TestKitEmbeddingConfigRequiresCredentialSource(t *testing.T) {
	const variable = "DOCBANK_TEST_UNAVAILABLE_KIT_KEY"
	t.Setenv(variable, "")
	cfg, descriptor := syntheticLoopbackOpenAIConfig(t, "http://127.0.0.1:11434", variable)
	p := cfg.EmbeddingProfiles["semantic"]
	p.Embedder = &embedconfig.Embedder{
		BaseURL: p.Runtime.Endpoint + "/v1", Model: p.Model, Dims: p.Dimensions,
		FingerprintSalt: p.Runtime.ModelRevision, BatchSize: p.MaxBatchItems, TimeoutSeconds: 1,
	}
	cfg.EmbeddingProfiles["semantic"] = p
	cfg.CredentialBindings = nil
	require.ErrorContains(t, cfg.Validate(), "runtime credential binding \"credential:semantic\" is not defined")
	_, err := configureEmbeddingRuntimeBundle(cfg, unavailableEmbeddingBlobs{}, t.TempDir())
	require.EqualError(t, err, "configuring embedding runtime \"semantic\": credential source is not configured")
	assert.Equal(t, "credential:semantic", cfg.EmbeddingProfiles["semantic"].CredentialBinding)

	// Declaring the source restores the existing descriptor without reading
	// the unavailable secret during startup.
	p.Embedder.APIKey = secretref.Ref{Env: variable}
	require.NoError(t, cfg.Validate())
	bundle, err := configureEmbeddingRuntimeBundle(cfg, unavailableEmbeddingBlobs{}, t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, descriptor, bundle.providers["semantic"].Descriptor())
}
