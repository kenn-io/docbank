package embedclient_test

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedmodel"
)

// These tests exercise the text transport with synthetic vectors. They do
// not verify a model's tokenizer, pooling, checkpoint, or serving behavior.
func TestEmbedderTextContract(t *testing.T) {
	for _, test := range []struct {
		name              string
		dims              int
		requestDimensions bool
	}{
		{"native", 768, false},
		{"requested native", 768, true},
		{"requested reduced", 512, true},
		{"server default reduced", 512, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := make(chan map[string]any, 1)
			values := make([]float32, test.dims)
			values[0], values[1] = 3, 4
			client := configuredTextClient(t, test.dims, test.requestDimensions, func(w http.ResponseWriter, r *http.Request) {
				requests <- readBody(t, r)
				writeJSON(t, w, map[string]any{
					"data": []map[string]any{{"index": 0, "embedding": values}},
				})
			})
			for _, input := range []struct {
				role     embedconfig.Role
				prepared bool
				text     string
				want     string
			}{
				{embedconfig.RoleDocument, false, "hello", "title: none | text: hello \n"},
				{embedconfig.RoleQuery, false, "hello", "task: search result | query: hello\t "},
				{embedconfig.RoleDocument, true, "title: none | text: hello \n", "title: none | text: hello \n"},
				{embedconfig.RoleQuery, true, "task: search result | query: hello\t ", "task: search result | query: hello\t "},
			} {
				var vectors [][]float32
				var err error
				if input.prepared {
					vectors, err = client.EncodeFunc(input.role)(t.Context(), []string{input.text})
				} else {
					vectors, err = client.EmbedTexts(t.Context(), input.role, []string{input.text})
				}
				require.NoError(t, err)
				require.Len(t, vectors, 1)
				require.Len(t, vectors[0], test.dims)
				assert.InDelta(t, 0.6, vectors[0][0], 1e-6)
				assert.InDelta(t, 0.8, vectors[0][1], 1e-6)
				var norm float64
				for _, value := range vectors[0] {
					assert.False(t, math.IsNaN(float64(value)) || math.IsInf(float64(value), 0))
					norm += float64(value) * float64(value)
				}
				assert.InDelta(t, 1.0, norm, 1e-6)
				body := <-requests
				assert.Equal(t, "embed-text", body["model"])
				assert.Equal(t, []any{input.want}, body["input"])
				assert.NotContains(t, body, "input_type")
				if test.requestDimensions {
					assert.InDelta(t, float64(test.dims), body["dimensions"], 0)
				} else {
					assert.NotContains(t, body, "dimensions")
				}
			}
		})
	}
}

func TestEmbedderRejectsInvalidTextVectors(t *testing.T) {
	nonFinite := make([]byte, 768*4)
	binary.LittleEndian.PutUint32(nonFinite, math.Float32bits(float32(math.Inf(1))))
	native := make([]float32, 768)
	native[0] = 1
	nullComponent := make([]any, 768)
	for i := range nullComponent {
		nullComponent[i] = 0
	}
	nullComponent[0], nullComponent[1] = nil, 1
	for _, test := range []struct {
		name              string
		dims              int
		requestDimensions bool
		embedding         any
	}{
		{"wrong width", 768, false, []float32{3, 4}},
		{"native width instead of requested width", 512, true, native},
		{"zero norm", 768, false, make([]float32, 768)},
		{"null component", 768, false, nullComponent},
		{"nonfinite component", 768, false, base64.StdEncoding.EncodeToString(nonFinite)},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := configuredTextClient(t, test.dims, test.requestDimensions, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, map[string]any{
					"data": []map[string]any{{"index": 0, "embedding": test.embedding}},
				})
			})
			vectors, err := client.EmbedTexts(t.Context(), embedconfig.RoleDocument, []string{"hello"})
			require.ErrorIs(t, err, embedclient.ErrInvalidVector)
			assert.Nil(t, vectors)
		})
	}
}

func TestEmbedderTextContractRejectsNonText(t *testing.T) {
	client := configuredTextClient(t, 768, false, func(_ http.ResponseWriter, _ *http.Request) {
		assert.Fail(t, "non-text input must be rejected before HTTP")
	})
	for _, kind := range []string{embedmodel.KindImage, embedmodel.KindFile} {
		vectors, err := client.Embed(t.Context(), []embedmodel.Content{{
			Role: embedconfig.RoleDocument, Kind: kind, Text: "hello",
		}})
		require.ErrorIs(t, err, embedmodel.ErrUnsupportedContent)
		assert.Nil(t, vectors)
	}
}

func configuredTextClient(t *testing.T, dims int, requestDimensions bool, handler http.HandlerFunc) *embedclient.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	var config embedconfig.Embedder
	meta, err := toml.Decode(fmt.Sprintf(`
base_url = %q
model = "embed-text"
dims = %d
document_prefix = "title: none | text: "
document_suffix = " \n"
query_prefix = "task: search result | query: "
query_suffix = "\t "
request_dimensions = %t
`, server.URL, dims, requestDimensions), &config)
	require.NoError(t, err)
	require.Empty(t, meta.Undecoded())
	parts, err := config.Parts()
	require.NoError(t, err)
	client, err := embedclient.New(embedclient.Options{
		Model: parts.Model, Roles: parts.Roles, Deployment: parts.Deployment,
		Batch: parts.Batch, Transport: parts.Transport,
	})
	require.NoError(t, err)
	return client
}
