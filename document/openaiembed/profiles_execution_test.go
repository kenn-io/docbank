package openaiembed

import (
	"bytes"
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestReviewedProfilesExecuteThroughCurrentCore(t *testing.T) {
	tests := []struct {
		name      string
		build     func() (Profile, error)
		dimension int
		wantInput []string
	}{
		{
			name: "bge-m3", build: func() (Profile, error) { return BGEM3Profile(reviewedProfileConfig()) },
			dimension: 1024, wantInput: []string{"passage", "question"},
		},
		{
			name: "qwen3-0.6b", build: func() (Profile, error) { return reviewedQwen3Profile(Qwen3Embedding06B) },
			dimension: 1024, wantInput: []string{"passage", "Instruct: Retrieve supporting passages\nQuery:question"},
		},
		{
			name: "qwen3-4b", build: func() (Profile, error) { return reviewedQwen3Profile(Qwen3Embedding4B) },
			dimension: 2560, wantInput: []string{"passage", "Instruct: Retrieve supporting passages\nQuery:question"},
		},
		{
			name: "qwen3-8b", build: func() (Profile, error) { return reviewedQwen3Profile(Qwen3Embedding8B) },
			dimension: 4096, wantInput: []string{"passage", "Instruct: Retrieve supporting passages\nQuery:question"},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			profile, err := testCase.build()
			require.NoError(t, err)
			var captured capturedRequest
			client := newTestClient(t, profile, nil, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				require.NoError(t, json.UnmarshalRead(request.Body, &captured, json.RejectUnknownMembers(true)))
				return jsonResponse(request, http.StatusOK, reviewedIndexedResponse(t, profile.Descriptor.Model, testCase.dimension)), nil
			}))

			result, err := document.ExecuteEmbedding(t.Context(), client, testInputs(), reviewedAuthorization(profile.Descriptor))
			require.NoError(t, err)
			assert.Equal(t, testCase.wantInput, captured.Input)
			assert.Equal(t, profile.Descriptor.Model, captured.Model)
			assert.Equal(t, "float", captured.EncodingFormat)
			require.Len(t, result.Vectors, 2)
			assert.Equal(t, "document-1", result.Vectors[0].Key)
			assert.Len(t, result.Vectors[0].Values, testCase.dimension)
			assert.InDelta(t, 1, result.Vectors[0].Values[0], 0)
			assert.Equal(t, "query-1", result.Vectors[1].Key)
			assert.Len(t, result.Vectors[1].Values, testCase.dimension)
			assert.InDelta(t, 1, result.Vectors[1].Values[1], 0)
		})
	}
}

func TestReviewedProfilesRejectVectorDimensionAndNormalizationDrift(t *testing.T) {
	t.Run("BGE-M3 wrong dimension", func(t *testing.T) {
		profile, err := BGEM3Profile(reviewedProfileConfig())
		require.NoError(t, err)
		vector := unitVector(profile.Descriptor.Dimension-1, 0)
		client := newTestClient(t, profile, nil, reviewedVectorTransport(t, profile.Descriptor.Model, vector))

		_, err = client.Embed(t.Context(), testInputs()[:1], reviewedAuthorization(profile.Descriptor))
		require.ErrorContains(t, err, "dimension")
	})

	t.Run("Qwen3 wrong normalization", func(t *testing.T) {
		profile, err := reviewedQwen3Profile(Qwen3Embedding4B)
		require.NoError(t, err)
		vector := unitVector(profile.Descriptor.Dimension, 0)
		vector[0] = 2
		client := newTestClient(t, profile, nil, reviewedVectorTransport(t, profile.Descriptor.Model, vector))

		_, err = client.Embed(t.Context(), testInputs()[:1], reviewedAuthorization(profile.Descriptor))
		require.ErrorContains(t, err, "normalization")
	})
}

func TestReviewedProfileRejectsNullVectorCoordinate(t *testing.T) {
	profile, err := BGEM3Profile(reviewedProfileConfig())
	require.NoError(t, err)
	client := newTestClient(t, profile, nil, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return jsonResponse(request, http.StatusOK, reviewedNullResponse(profile.Descriptor.Model, profile.Descriptor.Dimension)), nil
	}))

	_, err = client.Embed(t.Context(), testInputs()[:1], reviewedAuthorization(profile.Descriptor))
	require.ErrorContains(t, err, "bounded embedding schema")
}

func TestReviewedProfileOwnsValidatedInputsAcrossSynchronousCallback(t *testing.T) {
	config := reviewedProfileConfig()
	config.SecretBinding = "local-embedding-key"
	profile, err := BGEM3Profile(config)
	require.NoError(t, err)
	inputs := []document.EmbeddingInput{
		{Key: "document-1", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "passage", HeadingPath: []string{"Heading"}, SourceSpans: []document.ChunkSpan{{UnitIndex: 0, CharStart: 0, CharEnd: 1}}},
		{Key: "query-1", Role: document.EmbeddingRoleQuery, Kind: document.EmbeddingInputQueryText, Text: "question"},
	}
	var captured capturedRequest
	client := newTestClient(t, profile, mutatingSecretResolver(func() {
		inputs[0], inputs[1] = inputs[1], inputs[0]
		inputs[1].Key = "replacement-document"
		inputs[1].HeadingPath[0] = ""
		inputs[1].SourceSpans[0].CharEnd = 0
	}), roundTripFunc(func(request *http.Request) (*http.Response, error) {
		require.NoError(t, json.UnmarshalRead(request.Body, &captured, json.RejectUnknownMembers(true)))
		return jsonResponse(request, http.StatusOK, reviewedIndexedResponse(t, profile.Descriptor.Model, profile.Descriptor.Dimension)), nil
	}))

	result, err := client.Embed(t.Context(), inputs, reviewedAuthorization(profile.Descriptor))
	require.NoError(t, err)
	assert.Equal(t, []string{"passage", "question"}, captured.Input)
	require.Len(t, result.Vectors, 2)
	assert.Equal(t, "document-1", result.Vectors[0].Key)
	assert.Equal(t, "query-1", result.Vectors[1].Key)
}

func reviewedQwen3Profile(model Qwen3Model) (Profile, error) {
	return Qwen3Profile(Qwen3ProfileConfig{
		ReviewedProfileConfig: reviewedProfileConfig(), Model: model,
		QueryInstruction: "Retrieve supporting passages",
	})
}

func reviewedAuthorization(descriptor document.EmbeddingDescriptor) document.EmbeddingAuthorization {
	return document.EmbeddingAuthorization{
		ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
		PolicyFingerprint: descriptor.PolicyFingerprint, MaxBatchItems: 2,
		MaxInputBytes: 1024, MaxResponseBytes: 1 << 20,
	}
}

func reviewedIndexedResponse(t *testing.T, model string, dimension int) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"object": "list",
		"data": []map[string]any{
			{"object": "embedding", "embedding": unitVector(dimension, 1), "index": 1},
			{"object": "embedding", "embedding": unitVector(dimension, 0), "index": 0},
		},
		"model": model,
	})
	require.NoError(t, err)
	return body
}

func reviewedVectorTransport(t *testing.T, model string, vector []float32) http.RoundTripper {
	t.Helper()
	return roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return jsonResponse(request, http.StatusOK, singleVectorResponse(model, vector)), nil
	})
}

func unitVector(dimension, coordinate int) []float32 {
	vector := make([]float32, dimension)
	vector[coordinate] = 1
	return vector
}

func reviewedNullResponse(model string, dimension int) []byte {
	var vector bytes.Buffer
	vector.WriteString("[1,null")
	for range dimension - 2 {
		vector.WriteString(",0")
	}
	vector.WriteByte(']')
	return fmt.Appendf(nil, `{"object":"list","data":[{"object":"embedding","embedding":%s,"index":0}],"model":%q}`, vector.String(), model)
}
