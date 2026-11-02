package embedclient_test

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jsonv2 "encoding/json/v2"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedmodel"
	"go.kenn.io/kit/vector"
)

func TestEmbedReordersIndexedResponses(t *testing.T) {
	var got map[string]any
	client := newClient(t, embedconfig.Model{
		Name: "m", Dimensions: 2,
		Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationL2,
		RequestDimensions: true, EncodingFormat: "float",
	}, embedconfig.Roles{InputType: embedconfig.InputTypeRetrieval}, embedconfig.Batch{}, func(w http.ResponseWriter, r *http.Request) {
		got = readBody(t, r)
		writeJSON(t, w, map[string]any{
			"usage": map[string]any{"total_tokens": 3},
			"data": []map[string]any{
				{"index": 1, "embedding": []float64{0, 1}},
				{"index": 0, "embedding": []float64{1, 0}},
			},
		})
	})
	vectors, err := client.Embed(t.Context(), []embedmodel.Content{
		{Role: embedconfig.RoleDocument, Kind: embedmodel.KindText, Text: "alpha"},
		{Role: embedconfig.RoleDocument, Kind: embedmodel.KindText, Text: "beta"},
	})
	require.NoError(t, err)
	require.Len(t, vectors, 2)
	assert.InDeltaSlice(t, []float32{1, 0}, vectors[0], 1e-6)
	assert.InDeltaSlice(t, []float32{0, 1}, vectors[1], 1e-6)
	assert.Equal(t, "document", got["input_type"])
	dims, ok := got["dimensions"].(float64)
	require.True(t, ok)
	assert.InDelta(t, 2.0, dims, 0)
	assert.Equal(t, "float", got["encoding_format"])
}

func TestEmbedOmitsDimensionsUnlessRequested(t *testing.T) {
	var body map[string]any
	client := newClient(t, unitModel(), embedconfig.Roles{}, embedconfig.Batch{}, func(w http.ResponseWriter, r *http.Request) {
		body = readBody(t, r)
		writeJSON(t, w, map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": []float64{1, 0}}},
		})
	})
	_, err := client.Embed(t.Context(), oneText())
	require.NoError(t, err)
	assert.NotContains(t, body, "dimensions",
		"a provider without dimension selection must not receive the field")
	assert.NotContains(t, body, "encoding_format")
	assert.NotContains(t, body, "input_type")
}

func TestEmbedKeepsPositionalOrderWhenIndexIsAbsent(t *testing.T) {
	client := newClient(t, unitModel(), embedconfig.Roles{DocumentPrefix: "doc: "}, embedconfig.Batch{}, func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		_, indexed := body["input_type"]
		assert.False(t, indexed)
		inputs := body["input"].([]any)
		assert.Equal(t, "doc: alpha", inputs[0])
		writeJSON(t, w, map[string]any{
			"data": []map[string]any{{"embedding": []float64{1, 0}}},
		})
	})
	vectors, err := client.Embed(t.Context(), []embedmodel.Content{{
		Role: embedconfig.RoleDocument, Kind: embedmodel.KindText, Text: "alpha",
	}})
	require.NoError(t, err)
	assert.InDeltaSlice(t, []float32{1, 0}, vectors[0], 1e-6)
}

func TestEmbedRejectsBadResponses(t *testing.T) {
	tests := []struct {
		name string
		body map[string]any
	}{
		{name: "duplicate index", body: map[string]any{"data": []map[string]any{
			{"index": 0, "embedding": []float64{1, 0}},
			{"index": 0, "embedding": []float64{0, 1}},
		}}},
		{name: "mixed index", body: map[string]any{"data": []map[string]any{
			{"index": 0, "embedding": []float64{1, 0}},
			{"embedding": []float64{0, 1}},
		}}},
		{name: "short response", body: map[string]any{"data": []map[string]any{
			{"index": 0, "embedding": []float64{1, 0}},
		}}},
		{name: "wrong dimensions", body: map[string]any{"data": []map[string]any{
			{"index": 0, "embedding": []float64{1, 0, 0}},
			{"index": 1, "embedding": []float64{0, 1}},
		}}},
		{name: "null component", body: map[string]any{"data": []map[string]any{
			{"index": 0, "embedding": []any{1.0, nil}},
			{"index": 1, "embedding": []float64{0, 1}},
		}}},
		{name: "non finite", body: map[string]any{"data": []map[string]any{
			{"index": 0, "embedding": []float64{1e308, 0}},
			{"index": 1, "embedding": []float64{0, 1}},
		}}},
		{name: "zero norm", body: map[string]any{"data": []map[string]any{
			{"index": 0, "embedding": []float64{0, 0}},
			{"index": 1, "embedding": []float64{0, 1}},
		}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newClient(t, unitModel(), embedconfig.Roles{}, embedconfig.Batch{}, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, test.body)
			})
			_, err := client.Embed(t.Context(), twoTexts())
			require.Error(t, err)
		})
	}
}

func TestEmbedFailedResponseDoesNotEchoTheBody(t *testing.T) {
	client := newClient(t, unitModel(), embedconfig.Roles{}, embedconfig.Batch{}, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"secret prompt alpha"}`)
	})
	_, err := client.Embed(t.Context(), oneText())
	var api *embedclient.APIError
	require.ErrorAs(t, err, &api)
	assert.Equal(t, http.StatusTooManyRequests, api.StatusCode)
	assert.Equal(t, 3*time.Second, api.RetryAfter)
	assert.True(t, api.Retryable())
	assert.False(t, api.InputRejected())
	assert.NotContains(t, err.Error(), "secret")
	assert.NotContains(t, err.Error(), "alpha")

	denied := newClient(t, unitModel(), embedconfig.Roles{}, embedconfig.Batch{}, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "secret prompt alpha", http.StatusBadRequest)
	})
	_, err = denied.Embed(t.Context(), oneText())
	require.ErrorAs(t, err, &api)
	assert.False(t, api.Retryable())
	assert.False(t, api.InputRejected(), "a 400 that names no input problem is not an input rejection")
	assert.Equal(t, embedclient.ReasonUnknown, api.Reason)
	assert.False(t, api.CredentialsRejected())
	assert.NotContains(t, err.Error(), "secret")

	unauthorized := newClient(t, unitModel(), embedconfig.Roles{}, embedconfig.Batch{}, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "secret prompt alpha", http.StatusUnauthorized)
	})
	_, err = unauthorized.Embed(t.Context(), oneText())
	require.ErrorAs(t, err, &api)
	assert.False(t, api.Retryable())
	assert.False(t, api.InputRejected())
	assert.True(t, api.CredentialsRejected())
	assert.NotContains(t, err.Error(), "secret")
}

func TestEmbedBatchesByRoleAndRequestSize(t *testing.T) {
	var sizes []int
	client := newClient(t, unitModel(), embedconfig.Roles{
		DocumentPrefix: "doc: ",
		QueryPrefix:    "query: ",
	}, embedconfig.Batch{Items: 2}, func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		inputs := body["input"].([]any)
		sizes = append(sizes, len(inputs))
		data := make([]map[string]any, len(inputs))
		for i := range inputs {
			embedding := []float64{1, 0}
			if i == 1 {
				embedding = []float64{0, 1}
			}
			data[len(inputs)-1-i] = map[string]any{"index": i, "embedding": embedding}
		}
		writeJSON(t, w, map[string]any{"data": data})
	})
	vectors, err := client.Embed(t.Context(), []embedmodel.Content{
		{Role: embedconfig.RoleDocument, Kind: embedmodel.KindText, Text: "one"},
		{Role: embedconfig.RoleDocument, Kind: embedmodel.KindText, Text: "two"},
		{Role: embedconfig.RoleDocument, Kind: embedmodel.KindText, Text: "three"},
		{Role: embedconfig.RoleQuery, Kind: embedmodel.KindText, Text: "four"},
	})
	require.NoError(t, err)
	assert.Equal(t, []int{2, 1, 1}, sizes)
	require.Len(t, vectors, 4)
	assert.InDeltaSlice(t, []float32{1, 0}, vectors[0], 1e-6)
	assert.InDeltaSlice(t, []float32{0, 1}, vectors[1], 1e-6)
	assert.InDeltaSlice(t, []float32{1, 0}, vectors[2], 1e-6)
	assert.InDeltaSlice(t, []float32{1, 0}, vectors[3], 1e-6)
}

func TestEmbedKeepsUnnormalizedVectors(t *testing.T) {
	model := unitModel()
	model.Normalization = embedconfig.NormalizationNone
	client := newClient(t, model, embedconfig.Roles{}, embedconfig.Batch{}, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": []float64{3, 4}}},
		})
	})
	vectors, err := client.Embed(t.Context(), oneText())
	require.NoError(t, err)
	assert.InDeltaSlice(t, []float32{3, 4}, vectors[0], 1e-5)
}

func TestEmbedUsesCallerClientWithoutMutatingIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer resolved-key", r.Header.Get("Authorization"))
		writeJSON(t, w, map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": []float64{1, 0}}},
		})
	}))
	t.Cleanup(srv.Close)
	owned := &http.Client{Timeout: 5 * time.Second}
	client, err := embedclient.New(embedclient.Options{
		Model:      unitModel(),
		Deployment: embedconfig.Deployment{BaseURL: srv.URL},
		APIKey:     "resolved-key",
		HTTP:       owned,
	})
	require.NoError(t, err)
	_, err = client.Embed(t.Context(), oneText())
	require.NoError(t, err)
	assert.Nil(t, owned.Transport)
	assert.Equal(t, 5*time.Second, owned.Timeout)
}

func TestEmbedRefusesCrossOriginRedirect(t *testing.T) {
	client := newClient(t, unitModel(), embedconfig.Roles{}, embedconfig.Batch{}, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.test/embeddings", http.StatusFound)
	})
	_, err := client.Embed(t.Context(), oneText())
	require.ErrorIs(t, err, embedclient.ErrOrigin)
}

func TestEmbedStripsTransportDetails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	owned := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial secret-host.example failed")
	})}
	client, err := embedclient.New(embedclient.Options{
		Model:      unitModel(),
		Deployment: embedconfig.Deployment{BaseURL: srv.URL},
		APIKey:     "resolved-key",
		HTTP:       owned,
	})
	require.NoError(t, err)
	_, err = client.Embed(t.Context(), oneText())
	require.EqualError(t, err, "embed request failed")
	var transport *embedclient.TransportError
	require.ErrorAs(t, err, &transport)
	require.ErrorContains(t, transport.Err, "secret-host.example", "the cause stays reachable for retry decisions")

	owned.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, context.Canceled
	})
	client, err = embedclient.New(embedclient.Options{
		Model:      unitModel(),
		Deployment: embedconfig.Deployment{BaseURL: srv.URL},
		HTTP:       owned,
	})
	require.NoError(t, err)
	_, err = client.Embed(t.Context(), oneText())
	require.ErrorIs(t, err, context.Canceled)
	assert.NotContains(t, err.Error(), srv.URL)
}

func TestEmbedRejectsBlankAndNonTextBeforeTheRequest(t *testing.T) {
	called := false
	client := newClient(t, unitModel(), embedconfig.Roles{}, embedconfig.Batch{}, func(http.ResponseWriter, *http.Request) {
		called = true
	})
	_, err := client.Embed(t.Context(), []embedmodel.Content{{
		Role: embedconfig.RoleDocument, Kind: embedmodel.KindText, Text: " \u200b",
	}})
	require.ErrorIs(t, err, vector.ErrEmptyEmbeddingInput)
	_, err = client.Embed(t.Context(), []embedmodel.Content{{
		Role: embedconfig.RoleDocument,
		Kind: embedmodel.KindImage,
		Parts: []embedmodel.Part{{
			Kind: embedmodel.KindImage, MediaType: "image/png",
		}},
	}})
	require.ErrorIs(t, err, embedmodel.ErrUnsupportedContent)
	assert.False(t, called)
}

func TestEncodeFuncSendsPreparedTextUnchanged(t *testing.T) {
	var got []any
	client := newClient(t, unitModel(), embedconfig.Roles{DocumentPrefix: "doc: ", DocumentSuffix: " END"}, embedconfig.Batch{Items: 4}, func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		got = body["input"].([]any)
		writeJSON(t, w, map[string]any{"data": []map[string]any{
			{"embedding": []float64{1, 0}},
		}})
	})
	_, err := client.EncodeFunc(embedconfig.RoleDocument)(t.Context(), []string{"doc: ab END"})
	require.NoError(t, err)
	require.Equal(t, []any{"doc: ab END"}, got)
}

func TestEncodeFuncPreservesOrder(t *testing.T) {
	client := newClient(t, unitModel(), embedconfig.Roles{}, embedconfig.Batch{Items: 8}, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"data": []map[string]any{
			{"index": 1, "embedding": []float64{0, 1}},
			{"index": 0, "embedding": []float64{1, 0}},
		}})
	})
	vectors, err := client.EncodeFunc(embedconfig.RoleQuery)(t.Context(), []string{"alpha", "beta"})
	require.NoError(t, err)
	assert.InDeltaSlice(t, []float32{1, 0}, vectors[0], 1e-6)
	assert.InDeltaSlice(t, []float32{0, 1}, vectors[1], 1e-6)
}

func TestEmbedKeepsADeadlineThatHitsWhileReadingTheBody(t *testing.T) {
	client := newClient(t, unitModel(), embedconfig.Roles{}, embedconfig.Batch{}, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[`)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // stall mid-body until the client gives up
	})
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond) //nolint:kennlint // deadlinetest: the deadline is the expected result; the server stalls mid-body until the client gives up
	defer cancel()
	_, err := client.Embed(ctx, oneText())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	var transport *embedclient.TransportError
	require.ErrorAs(t, err, &transport)
}

func TestEmbedRejectsOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[1,0]}]}`)
	}))
	t.Cleanup(srv.Close)
	limited, err := embedclient.New(embedclient.Options{
		Model:      unitModel(),
		Deployment: embedconfig.Deployment{BaseURL: srv.URL},
		Transport:  embedconfig.Transport{MaxResponseBytes: 8},
	})
	require.NoError(t, err)
	_, err = limited.Embed(t.Context(), oneText())
	require.ErrorIs(t, err, embedclient.ErrResponseTooLarge)
}

func TestEmbedReportsABadVectorAtTheCallerIndex(t *testing.T) {
	requests := 0
	client := newClient(t, unitModel(), embedconfig.Roles{}, embedconfig.Batch{Items: 2}, func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		requests++
		if requests == 2 {
			_, _ = io.WriteString(w, `{"data":[{"embedding":[null,1]}]}`)
			return
		}
		data := make([]map[string]any, len(body["input"].([]any)))
		for i := range data {
			data[i] = map[string]any{"index": i, "embedding": []float64{1, 0}}
		}
		writeJSON(t, w, map[string]any{"data": data})
	})
	inputs := append(twoTexts(), embedmodel.Content{Role: embedconfig.RoleDocument, Text: "gamma"})
	_, err := client.Embed(t.Context(), inputs)
	require.ErrorIs(t, err, embedclient.ErrInvalidVector)
	var vectorErr *embedclient.VectorError
	require.ErrorAs(t, err, &vectorErr)
	assert.Equal(t, 2, vectorErr.Index, "the third input, not the first of the second request")
}

func TestEmbedTextsAppliesTheRoleAffixes(t *testing.T) {
	var sent []any
	client := newClient(t, unitModel(), embedconfig.Roles{QueryPrefix: "q: "}, embedconfig.Batch{}, func(w http.ResponseWriter, r *http.Request) {
		sent = readBody(t, r)["input"].([]any)
		writeJSON(t, w, map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float64{1, 0}}}})
	})
	vectors, err := client.EmbedTexts(t.Context(), embedconfig.RoleQuery, []string{"alpha"})
	require.NoError(t, err)
	require.Len(t, vectors, 1)
	assert.Equal(t, []any{"q: alpha"}, sent)
}

func TestEmbedSplitsOneRoleWhenTokenBudgetIsSmall(t *testing.T) {
	var sizes []int
	client := newClient(t, unitModel(), embedconfig.Roles{}, embedconfig.Batch{
		Items: 8, MaxTokens: 5, InputTokenUpperBound: 2,
	}, func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		inputs := body["input"].([]any)
		sizes = append(sizes, len(inputs))
		data := make([]map[string]any, len(inputs))
		for i := range inputs {
			data[i] = map[string]any{"index": i, "embedding": []float64{1, 0}}
		}
		writeJSON(t, w, map[string]any{"data": data})
	})
	vectors, err := client.Embed(t.Context(), []embedmodel.Content{
		{Role: embedconfig.RoleDocument, Kind: embedmodel.KindText, Text: "one"},
		{Role: embedconfig.RoleDocument, Kind: embedmodel.KindText, Text: "two"},
		{Role: embedconfig.RoleDocument, Kind: embedmodel.KindText, Text: "three"},
	})
	require.NoError(t, err)
	assert.Equal(t, []int{2, 1}, sizes)
	require.Len(t, vectors, 3)
}

func TestEmbedDecodesBase64Float32Payload(t *testing.T) {
	want := []float32{1.25, -3.5}
	raw := make([]byte, 8)
	binary.LittleEndian.PutUint32(raw[0:4], math.Float32bits(want[0]))
	binary.LittleEndian.PutUint32(raw[4:8], math.Float32bits(want[1]))
	encoded := base64.StdEncoding.EncodeToString(raw)
	model := unitModel()
	model.Normalization = embedconfig.NormalizationNone
	model.EncodingFormat = "base64"
	client := newClient(t, model, embedconfig.Roles{}, embedconfig.Batch{}, func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		assert.Equal(t, "base64", body["encoding_format"])
		writeJSON(t, w, map[string]any{
			"data": []map[string]any{{"embedding": encoded}},
		})
	})
	vectors, err := client.Embed(t.Context(), oneText())
	require.NoError(t, err)
	require.Len(t, vectors, 1)
	assert.InDeltaSlice(t, want, vectors[0], 0)
}

func TestNewRejectsNonCosineMetric(t *testing.T) {
	for _, metric := range []embedconfig.Metric{embedconfig.MetricDotProduct, embedconfig.MetricL2} {
		t.Run(string(metric), func(t *testing.T) {
			model := unitModel()
			model.Metric = metric
			_, err := embedclient.New(embedclient.Options{
				Model:      model,
				Deployment: embedconfig.Deployment{BaseURL: "https://example.test/v1"},
			})
			require.EqualError(t, err, "embed model metric must be cosine; dot_product and l2 are not storable yet")
		})
	}
}

func TestNewRejectsUnsupportedEncodingFormat(t *testing.T) {
	model := unitModel()
	model.EncodingFormat = "bytes"
	_, err := embedclient.New(embedclient.Options{
		Model:      model,
		Deployment: embedconfig.Deployment{BaseURL: "https://example.test/v1"},
	})
	require.EqualError(t, err, "embed encoding format must be empty, float, or base64")
}

func TestNewRejectsTokenBoundAboveTheBudget(t *testing.T) {
	_, err := embedclient.New(embedclient.Options{
		Model:      unitModel(),
		Deployment: embedconfig.Deployment{BaseURL: "https://example.test/v1"},
		Batch:      embedconfig.Batch{Items: 4, MaxTokens: 3, InputTokenUpperBound: 4},
	})
	require.EqualError(t, err, "embed batch per-input token bound must fit in the aggregate cap")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newClient(
	t *testing.T,
	model embedconfig.Model,
	roles embedconfig.Roles,
	batch embedconfig.Batch,
	handler http.HandlerFunc,
) *embedclient.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client, err := embedclient.New(embedclient.Options{
		Model:      model,
		Roles:      roles,
		Deployment: embedconfig.Deployment{BaseURL: srv.URL},
		Batch:      batch,
	})
	require.NoError(t, err)
	return client
}

func unitModel() embedconfig.Model {
	return embedconfig.Model{
		Name: "m", Dimensions: 2,
		Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationL2,
	}
}

func oneText() []embedmodel.Content {
	return []embedmodel.Content{{
		Role: embedconfig.RoleDocument, Kind: embedmodel.KindText, Text: "alpha",
	}}
}

func twoTexts() []embedmodel.Content {
	return []embedmodel.Content{
		{Role: embedconfig.RoleDocument, Kind: embedmodel.KindText, Text: "alpha"},
		{Role: embedconfig.RoleDocument, Kind: embedmodel.KindText, Text: "beta"},
	}
}

func readBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	defer func() { _ = r.Body.Close() }()
	payload, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, jsonv2.Unmarshal(payload, &body))
	return body
}

func writeJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	payload, err := jsonv2.Marshal(body)
	require.NoError(t, err)
	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(payload)
	require.NoError(t, err)
}

// TestEmbedClassifiesFailedResponses pins how a failed response becomes a
// Reason: the status decides most classes, and for 400, 413, and 422 the
// provider's error code or message separates an input the endpoint refuses
// from a request it never accepts. Only an input rejection may skip a
// document, so an unrecognized 400 must not become one.
func TestEmbedClassifiesFailedResponses(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   embedclient.Reason
	}{
		{
			"structured context length code", http.StatusBadRequest,
			`{"error":{"message":"This model's maximum context length is 8192 tokens","type":"invalid_request_error","code":"context_length_exceeded"}}`,
			embedclient.ReasonInputTooLong,
		},
		{
			"structured model code", http.StatusBadRequest,
			`{"error":{"message":"The model does not exist","code":"model_not_found"}}`,
			embedclient.ReasonInvalidRequest,
		},
		{
			"plain error string over the context", http.StatusBadRequest,
			`{"error":"maximum context length is 8192 tokens"}`, embedclient.ReasonInputTooLong,
		},
		{"text body token overflow", http.StatusBadRequest, "token window overflow", embedclient.ReasonInputTooLong},
		{"payload too large", http.StatusRequestEntityTooLarge, "input too large", embedclient.ReasonInputTooLong},
		{"payload too large without a body", http.StatusRequestEntityTooLarge, "", embedclient.ReasonInputTooLong},
		{"content policy", http.StatusUnprocessableEntity, "content policy violation", embedclient.ReasonContentPolicy},
		{
			"unsupported dimensions", http.StatusBadRequest,
			`{"error":"input exceeds supported dimensions"}`, embedclient.ReasonInvalidRequest,
		},
		{"unknown model", http.StatusBadRequest, `{"error":"model \"x\" not found"}`, embedclient.ReasonInvalidRequest},
		// Bodies below follow the error shapes each server sends.
		{
			"openai context length with a null code", http.StatusBadRequest,
			`{"error":{"message":"Invalid 'input': maximum context length is 8192 tokens.","type":"invalid_request_error","param":null,"code":null}}`,
			embedclient.ReasonInputTooLong,
		},
		{
			"openai unsupported dimensions", http.StatusBadRequest,
			`{"error":{"message":"This model does not support specifying dimensions.","type":"invalid_request_error","param":null,"code":null}}`,
			embedclient.ReasonInvalidRequest,
		},
		{
			"openai model without access", http.StatusForbidden,
			`{"error":{"message":"The model does not exist or you do not have access to it.","code":"model_not_found"}}`,
			embedclient.ReasonInvalidRequest,
		},
		{
			"ollama native context length", http.StatusBadRequest,
			`{"error":"the input length exceeds the context length"}`, embedclient.ReasonInputTooLong,
		},
		{
			"ollama openai route invalid input", http.StatusBadRequest,
			`{"error":{"message":"invalid input","type":"invalid_request_error","param":null,"code":null}}`,
			embedclient.ReasonUnknown,
		},
		{
			"tei token limit", http.StatusUnprocessableEntity,
			`{"message":"Input validation error: ` + "`inputs`" + ` must have less than 512 tokens. Given: 600","code":422,"type":"Validation"}`,
			embedclient.ReasonInputTooLong,
		},
		{
			"tei batch limit", http.StatusUnprocessableEntity,
			`{"message":"batch size 64 > maximum allowed batch size 32","code":422,"type":"Validation"}`,
			embedclient.ReasonUnknown,
		},
		{
			"tei plain text rejection", http.StatusUnprocessableEntity,
			"Failed to deserialize the JSON body into the target type", embedclient.ReasonUnknown,
		},
		{
			"vllm context length", http.StatusBadRequest,
			`{"error":{"message":"This model's maximum context length is 512 tokens. However, you requested 600 tokens.","type":"BadRequestError","param":"input_tokens","code":400}}`,
			embedclient.ReasonInputTooLong,
		},
		{
			"vllm matryoshka dimensions", http.StatusBadRequest,
			`{"error":{"message":"Model 'x' does not support Matryoshka embeddings; dimensions must be unset","type":"BadRequestError","param":null,"code":400}}`,
			embedclient.ReasonInvalidRequest,
		},
		{
			"litellm context window", http.StatusBadRequest,
			`{"error":{"message":"litellm.ContextWindowExceededError: input too long","type":null,"param":null,"code":"400"}}`,
			embedclient.ReasonInputTooLong,
		},
		{
			"request input count", http.StatusBadRequest,
			`{"error":{"message":"Too many inputs. The max number of inputs is 2048.","code":null}}`,
			embedclient.ReasonUnknown,
		},
		{
			"request token total", http.StatusBadRequest,
			`{"error":{"message":"Requested 400000 tokens, max 300000 tokens per request","code":null}}`,
			embedclient.ReasonUnknown,
		},
		{"empty 400", http.StatusBadRequest, "", embedclient.ReasonUnknown},
		{"route text", http.StatusBadRequest, "no route", embedclient.ReasonUnknown},
		{"invalid token wording", http.StatusBadRequest, "invalid token", embedclient.ReasonUnknown},
		{"unsupported content type", http.StatusUnprocessableEntity, "unsupported content type", embedclient.ReasonUnknown},
		{"not found", http.StatusNotFound, "model not found", embedclient.ReasonInvalidRequest},
		{"unauthorized", http.StatusUnauthorized, "invalid token", embedclient.ReasonCredentials},
		{"forbidden", http.StatusForbidden, "forbidden", embedclient.ReasonCredentials},
		{"rate limited", http.StatusTooManyRequests, "input rate limited", embedclient.ReasonRateLimited},
		{"server error", http.StatusInternalServerError, "token error", embedclient.ReasonUnavailable},
		{"bad gateway", http.StatusBadGateway, "", embedclient.ReasonUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newClient(t, unitModel(), embedconfig.Roles{}, embedconfig.Batch{}, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			})
			_, err := client.Embed(t.Context(), oneText())
			api, ok := errors.AsType[*embedclient.APIError](err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, test.want, api.Reason)
			assert.Equal(t, test.want == embedclient.ReasonInputTooLong || test.want == embedclient.ReasonContentPolicy,
				api.InputRejected())
			assert.Contains(t, err.Error(), test.want.String())
		})
	}
}
