package embedding

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientBatchesAuthenticatesOrdersAndNormalizes(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Equal(t, "/v1/embeddings", r.URL.Path)
		assert.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
		var request embedRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decoding embedding request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		data := make([]map[string]any, len(request.Input))
		for i := range request.Input {
			data[len(request.Input)-1-i] = map[string]any{
				"index": i, "embedding": []float32{3, float32(4 + i)},
			}
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			t.Errorf("encoding embedding response: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{
		BaseURL: server.URL + "/v1", Model: "model", APIKey: "secret",
		Dimensions: 2, BatchSize: 2, Timeout: time.Second,
	})
	require.NoError(t, err)
	vectors, err := client.Embed(t.Context(), []string{"a", "b", "c"})
	require.NoError(t, err)
	require.Len(t, vectors, 3)
	assert.Equal(t, 2, requests)
	for _, vector := range vectors {
		norm := math.Sqrt(float64(vector[0]*vector[0] + vector[1]*vector[1]))
		assert.InDelta(t, 1, norm, 1e-6)
	}
}

func TestClientRejectsMalformedVectors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[null,1]}]}`))
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{
		BaseURL: server.URL, Model: "model", Dimensions: 2, BatchSize: 1, Timeout: time.Second,
	})
	require.NoError(t, err)
	_, err = client.Embed(t.Context(), []string{"a"})
	assert.ErrorContains(t, err, "component 0 is null")
}

func TestGenerationFingerprintExcludesEndpointAndIncludesSalt(t *testing.T) {
	base := Config{BaseURL: "https://one.example/v1", Model: "model",
		Dimensions: 3, BatchSize: 1, Timeout: time.Second}
	first, err := New(base)
	require.NoError(t, err)
	base.BaseURL = "https://two.example/v1"
	second, err := New(base)
	require.NoError(t, err)
	assert.Equal(t, first.Generation().Fingerprint(), second.Generation().Fingerprint())
	base.FingerprintSalt = "new weights"
	third, err := New(base)
	require.NoError(t, err)
	assert.NotEqual(t, first.Generation().Fingerprint(), third.Generation().Fingerprint())
}
