package api_test

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitvec "go.kenn.io/kit/vector"

	"go.kenn.io/docbank/internal/api"
	docvector "go.kenn.io/docbank/internal/vector"
)

func TestEmbeddingRoutesExposeConfigurationAndStreamBuild(t *testing.T) {
	index, err := docvector.Open(t.Context(), filepath.Join(t.TempDir(), "vectors.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })
	documents := []docvector.Document{
		{BlobHash: strings.Repeat("a", 64), ExtractorVersion: 1, Text: "alpha"},
		{BlobHash: strings.Repeat("b", 64), ExtractorVersion: 1, Text: "beta"},
	}
	service := &docvector.Service{
		Index: index,
		Source: func(_ context.Context, after string, limit int) ([]docvector.Document, error) {
			var page []docvector.Document
			for _, item := range documents {
				if item.BlobHash > after && len(page) < limit {
					page = append(page, item)
				}
			}
			return page, nil
		},
		Generation: kitvec.Generation{Model: "test-model", Dimensions: 2},
		Encode: func(_ context.Context, texts []string) ([][]float32, error) {
			vectors := make([][]float32, len(texts))
			for i, text := range texts {
				second := float32(len(text)) / 100
				scale := float32(1 / math.Sqrt(float64(1+second*second)))
				vectors[i] = []float32{scale, second * scale}
			}
			return vectors, nil
		},
		BatchSize: 8, Concurrency: 1,
	}
	ts, _ := newTestServer(t, func(d *api.Deps) { d.Embeddings = service })

	resp, body := do(t, ts, http.MethodPost, "/api/v1/embeddings/build/stream", nil, map[string]any{})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/x-ndjson")
	events := decodeEmbeddingEvents(t, body)
	require.NotEmpty(t, events)
	assert.Equal(t, "progress", events[0].Type)
	terminal := events[len(events)-1]
	require.Equal(t, "result", terminal.Type)
	require.NotNil(t, terminal.Result)
	assert.True(t, terminal.Result.Activated)
	assert.Equal(t, 2, terminal.Result.Embedded)

	resp, body = get(t, ts, "/api/v1/embeddings", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var listed api.EmbeddingGenerationList
	require.NoError(t, json.Unmarshal([]byte(body), &listed))
	assert.True(t, listed.Configured)
	require.Len(t, listed.Items, 1)
	assert.Equal(t, "active", listed.Items[0].State)
	assert.Equal(t, 2, listed.Items[0].Embedded)
}

func TestEmbeddingRoutesRemainHonestWhenUnconfigured(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, body := get(t, ts, "/api/v1/embeddings", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var listed api.EmbeddingGenerationList
	require.NoError(t, json.Unmarshal([]byte(body), &listed))
	assert.False(t, listed.Configured)
	assert.Empty(t, listed.Items)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/embeddings/build/stream", nil, map[string]any{})
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	assert.Contains(t, body, "embeddings_unconfigured")
}

func decodeEmbeddingEvents(t *testing.T, body string) []api.EmbeddingBuildEvent {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(body))
	var events []api.EmbeddingBuildEvent
	for {
		var event api.EmbeddingBuildEvent
		err := decoder.Decode(&event)
		if err == io.EOF {
			return events
		}
		require.NoError(t, err)
		events = append(events, event)
	}
}
