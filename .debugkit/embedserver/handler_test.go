package embedserver

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	jsonv1 "encoding/json"
	json "encoding/json/v2"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeEngine struct {
	mu      sync.Mutex
	inputs  []string
	run     func(context.Context, string) ([]float32, int, error)
	onClose func()
}

func (e *fakeEngine) embed(ctx context.Context, text string) ([]float32, int, error) {
	e.mu.Lock()
	e.inputs = append(e.inputs, text)
	e.mu.Unlock()
	if e.run != nil {
		return e.run(ctx, text)
	}
	v := make([]float32, 768)
	v[0] = 3
	v[1] = 4
	return v, 7, nil
}

func (e *fakeEngine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.onClose != nil {
		e.onClose()
	}
	return nil
}

func TestHandlerWire(t *testing.T) {
	inputs := make([]string, 64)
	for i := range inputs {
		inputs[i] = strconv.Itoa(i)
	}
	type wireCase struct {
		name, model, encoding string
		dimensions            int
		input                 any
		texts                 []string
	}
	cases := []wireCase{
		{"string/default", "nomic-embed-text:v1.5", "", 0, "search_query: 0", []string{"search_query: 0"}},
		{"array/default", "nomic-embed-text:latest", "", 0, []string{"search_query: 0", "search_document: 1"}, []string{"search_query: 0", "search_document: 1"}},
	}
	for _, encoding := range []string{"float", "base64"} {
		for _, dimensions := range []int{256, 768} {
			if encoding == "float" && dimensions == 768 {
				continue
			}
			cases = append(cases, wireCase{encoding + "/" + strconv.Itoa(dimensions), "nomic-embed-text:latest", encoding, dimensions, inputs, inputs})
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &fakeEngine{run: func(_ context.Context, text string) ([]float32, int, error) {
				index, err := strconv.Atoi(strings.TrimPrefix(strings.TrimPrefix(text, "search_query: "), "search_document: "))
				if err != nil {
					return nil, 0, err
				}
				vector := make([]float32, 768)
				vector[index], vector[767] = 1, 1
				return vector, 7, nil
			}}
			body, err := json.Marshal(map[string]any{"model": tc.model, "input": tc.input, "dimensions": tc.dimensions, "encoding_format": tc.encoding})
			require.NoError(t, err)
			recorder := httptest.NewRecorder()
			(&handler{engine: e, gate: make(chan struct{}, 1)}).ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/embeddings", strings.NewReader(string(body))))
			require.Equal(t, http.StatusOK, recorder.Code)
			var out struct {
				Model string `json:"model"`
				Usage usage  `json:"usage"`
				Data  []struct {
					Index     int               `json:"index"`
					Embedding jsonv1.RawMessage `json:"embedding"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &out))
			assert.Equal(t, tc.model, out.Model)
			assert.Equal(t, usage{7 * len(tc.texts), 7 * len(tc.texts)}, out.Usage)
			require.Len(t, out.Data, len(tc.texts))
			dimensions, expected := tc.dimensions, 1.0
			if dimensions == 0 || dimensions == 768 {
				dimensions, expected = 768, 1/math.Sqrt2
			}
			for i, item := range out.Data {
				var vector []float32
				if tc.encoding == "base64" {
					var encoded string
					require.NoError(t, json.Unmarshal(item.Embedding, &encoded))
					data, err := base64.StdEncoding.DecodeString(encoded)
					require.NoError(t, err)
					require.Len(t, data, dimensions*4)
					vector = make([]float32, dimensions)
					for j := range vector {
						vector[j] = math.Float32frombits(binary.LittleEndian.Uint32(data[j*4:]))
					}
				} else {
					require.NoError(t, json.Unmarshal(item.Embedding, &vector))
				}
				require.Len(t, vector, dimensions)
				assert.Equal(t, i, item.Index)
				assert.InDelta(t, expected, vector[i], 1e-6)
				var squared float64
				for _, value := range vector {
					squared += float64(value) * float64(value)
				}
				assert.InDelta(t, 1, squared, 1e-6)
			}
			assert.Equal(t, tc.texts, e.inputs)
		})
	}
}

func TestHandlerRejectsBeforeInference(t *testing.T) {
	cases := []struct {
		name, body, message string
		status              int
	}{
		{"malformed JSON", `{`, "invalid request", 400},
		{"unknown model", `{"model":"other","input":"x"}`, "model not found", 404},
		{"numeric input", `{"model":"nomic-embed-text","input":1}`, "input must be a string or string array", 400},
		{"empty input", `{"model":"nomic-embed-text","input":[]}`, "input must contain at least one string", 400},
		{"null input", `{"model":"nomic-embed-text","input":null}`, "input must contain at least one string", 400},
		{"null array item", `{"model":"nomic-embed-text","input":[null]}`, "input must contain strings", 400},
		{"negative dimensions", `{"model":"nomic-embed-text","input":"x","dimensions":-1}`, "dimensions must be between 0 and 768", 400},
		{"dimensions too large", `{"model":"nomic-embed-text","input":"x","dimensions":769}`, "dimensions must be between 0 and 768", 400},
		{"unsupported encoding", `{"model":"nomic-embed-text","input":"x","encoding_format":"int8"}`, "encoding_format must be float or base64", 400},
	}
	e := new(fakeEngine)
	h := &handler{engine: e, gate: make(chan struct{}, 1)}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/embeddings", strings.NewReader(tc.body)))
			assert.Equal(t, tc.status, recorder.Code)
			assert.Equal(t, tc.message+"\n", recorder.Body.String())
		})
	}
	assert.Empty(t, e.inputs)
}

func TestHandlerAcceptsOllamaRequestBodies(t *testing.T) {
	for _, tc := range []struct{ name, body, text string }{
		{"lone surrogate", `{"model":"nomic-embed-text","input":"cut \ud83d"}`, "cut \ufffd"},
		{"lone surrogate in array", `{"model":"nomic-embed-text","input":["cut \ud83d"]}`, "cut \ufffd"},
		{"invalid UTF-8", "{\"model\":\"nomic-embed-text\",\"input\":\"bad \xff\"}", "bad \ufffd"},
		{"duplicate member", `{"model":"nomic-embed-text","input":"first","input":"last"}`, "last"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := new(fakeEngine)
			recorder := httptest.NewRecorder()
			(&handler{engine: e, gate: make(chan struct{}, 1)}).ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/embeddings", strings.NewReader(tc.body)))
			assert.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			assert.Equal(t, []string{tc.text}, e.inputs)
		})
	}
}

func TestHandlerReportsInferenceFailure(t *testing.T) {
	e := &fakeEngine{run: func(context.Context, string) ([]float32, int, error) {
		return nil, 0, errors.New("ONNX Runtime: out of memory")
	}}
	var log strings.Builder
	recorder := httptest.NewRecorder()
	h := &handler{engine: e, gate: make(chan struct{}, 1), log: &log}
	h.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"nomic-embed-text","input":"x"}`)))
	assert.Equal(t, http.StatusInternalServerError, recorder.Code)
	assert.Equal(t, "inference failed\n", recorder.Body.String())
	assert.Equal(t, "inference failed: ONNX Runtime: out of memory\n", log.String())
}

func TestHandlerRunsRequestsBetweenBatchItems(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		e := &fakeEngine{run: func(_ context.Context, text string) ([]float32, int, error) {
			if text == "batch 0" {
				<-release
			}
			v := make([]float32, 768)
			v[0] = 1
			return v, 1, nil
		}}
		h := &handler{engine: e, gate: make(chan struct{}, 1)}
		var wg sync.WaitGroup
		post := func(body string) {
			wg.Go(func() {
				recorder := httptest.NewRecorder()
				h.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/embeddings", strings.NewReader(body)))
				assert.Equal(t, http.StatusOK, recorder.Code)
			})
		}
		post(`{"model":"nomic-embed-text","input":["batch 0","batch 1"]}`)
		synctest.Wait()
		post(`{"model":"nomic-embed-text","input":"query"}`)
		synctest.Wait()
		close(release)
		wg.Wait()
		assert.Equal(t, []string{"batch 0", "query", "batch 1"}, e.inputs)
	})
}

func TestHandlerEncodeFailure(t *testing.T) {
	e := &fakeEngine{run: func(context.Context, string) ([]float32, int, error) {
		vector := make([]float32, 768)
		vector[0] = float32(math.NaN())
		return vector, 7, nil
	}}
	recorder := httptest.NewRecorder()
	h := &handler{engine: e, gate: make(chan struct{}, 1)}
	h.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"nomic-embed-text","input":"x"}`)))
	assert.Equal(t, http.StatusInternalServerError, recorder.Code)
	assert.Equal(t, "encode failed\n", recorder.Body.String())
}

func TestHandlerLargeBatch(t *testing.T) {
	inputs := make([]string, 300)
	for i := range inputs {
		inputs[i] = strings.Repeat("x", 16<<10)
	}
	body, err := json.Marshal(map[string]any{"model": "nomic-embed-text", "input": inputs})
	require.NoError(t, err)
	require.Greater(t, len(body), 4<<20)
	recorder := httptest.NewRecorder()
	h := &handler{engine: new(fakeEngine), gate: make(chan struct{}, 1)}
	h.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/embeddings", strings.NewReader(string(body))))
	require.Equal(t, http.StatusOK, recorder.Code)
	var out struct {
		Data []jsonv1.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &out))
	assert.Len(t, out.Data, 300)
}

func TestHandlerCancellation(t *testing.T) {
	entered := make(chan struct{})
	e := &fakeEngine{run: func(ctx context.Context, _ string) ([]float32, int, error) {
		close(entered)
		<-ctx.Done()
		return nil, 0, ctx.Err()
	}}
	h := &handler{engine: e, gate: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request := func(ctx context.Context) *http.Request {
		return httptest.NewRequestWithContext(ctx, "POST", "/v1/embeddings", strings.NewReader(`{"model":"nomic-embed-text","input":"x"}`))
	}
	first, waiter := httptest.NewRecorder(), httptest.NewRecorder()
	first.Code, waiter.Code = 0, 0
	done := make(chan struct{})
	go func() { defer close(done); h.ServeHTTP(first, request(ctx)) }()
	<-entered
	waiting, stop := context.WithCancel(t.Context())
	stop()
	h.ServeHTTP(waiter, request(waiting))
	cancel()
	<-done
	assert.Zero(t, first.Code)
	assert.Empty(t, first.Body.String())
	assert.Zero(t, waiter.Code)
	assert.Empty(t, waiter.Body.String())
}

func TestHandlerGateRelease(t *testing.T) {
	t.Run("panic", func(t *testing.T) {
		e := &fakeEngine{run: func(context.Context, string) ([]float32, int, error) {
			panic("inference panic")
		}}
		h := &handler{engine: e, gate: make(chan struct{}, 1)}
		assert.PanicsWithValue(t, "inference panic", func() {
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"nomic-embed-text","input":"x"}`)))
		})
		assert.Empty(t, h.gate)
	})
	t.Run("blocked response", func(t *testing.T) {
		e := new(fakeEngine)
		h := &handler{engine: e, gate: make(chan struct{}, 1)}
		request := func(ctx context.Context) *http.Request {
			return httptest.NewRequestWithContext(ctx, "POST", "/v1/embeddings", strings.NewReader(`{"model":"nomic-embed-text","input":"x"}`))
		}
		writing, release := make(chan struct{}), make(chan struct{})
		writer := &blockedResponseWriter{ResponseRecorder: httptest.NewRecorder(), entered: writing, release: release}
		done := make(chan struct{})
		defer func() { close(release); <-done }()
		go func() { defer close(done); h.ServeHTTP(writer, request(t.Context())) }()
		select {
		case <-writing:
		case <-time.After(5 * time.Second):
			require.FailNow(t, "response did not reach blocked Write")
		}
		live, stopLive := context.WithTimeout(t.Context(), 5*time.Second)
		defer stopLive()
		next := httptest.NewRecorder()
		h.ServeHTTP(next, request(live))
		assert.NotEmpty(t, next.Body.String(), "blocked response retained inference gate")
		e.mu.Lock()
		defer e.mu.Unlock()
		assert.Equal(t, []string{"x", "x"}, e.inputs)
	})
}

type blockedResponseWriter struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (w *blockedResponseWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return w.ResponseRecorder.Write(p)
}
