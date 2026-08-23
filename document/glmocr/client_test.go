package glmocr_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/glmocr"
	"go.kenn.io/docbank/document/ocr"
)

func TestClientUsesStructuredPagesAndPreservesElements(t *testing.T) {
	var sawDataURI bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/glmocr/parse", r.URL.Path)
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		sawDataURI = strings.Contains(string(body), `data:image/png;base64,`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"model":"glm-ocr",
			"deployment_fingerprint":"`+glmocr.DefaultDeploymentFingerprint+`",
			"json_result":[
				[{"index":0,"label":"text","content":"# Synthetic archive","bbox_2d":[10,20,900,80]},
				 {"index":1,"label":"table","content":"<table><tr><th>Item</th><th>Count</th></tr><tr><td>Cedar</td><td>7</td></tr></table>","bbox_2d":[10,100,900,500]}],
				[{"index":0,"label":"formula","content":"$$\\nx^2 + y^2 = z^2\\n$$","bbox_2d":[100,100,800,300]}]
			]
		}`)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL+"/glmocr/parse", 1<<20, 2)
	source := syntheticPNGSource(t)

	result, err := client.Process(context.Background(), source)
	require.NoError(t, err)
	assert.True(t, sawDataURI)
	assert.Equal(t, "glmocr", result.Identity.Provider)
	assert.Equal(t, 2, result.UnitsProcessed)
	require.Len(t, result.Structure, 2)
	assert.Equal(t, "table", result.Structure[0].Elements[1].Kind)
	assert.Equal(t, "formula", result.Structure[1].Elements[0].Kind)
	require.Len(t, result.Document.Units, 2)
	assert.Contains(t, result.Document.Units[0].Text, "Item | Count")
	assert.Contains(t, result.Document.Units[1].Text, "x^2 + y^2 = z^2")
	assert.Equal(t, []string{"Synthetic archive"}, result.Document.Chunks[0].HeadingPath)
}

func TestClientRetriesTransientServiceFailure(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"glm-ocr","deployment_fingerprint":"`+glmocr.DefaultDeploymentFingerprint+`","json_result":[[{"index":0,"label":"text","content":"synthetic retry","bbox_2d":null}]]}`)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL+"/glmocr/parse", 1<<20, 2)

	result, err := client.Process(context.Background(), syntheticPNGSource(t))
	require.NoError(t, err)
	assert.Equal(t, 2, result.Metrics.Requests)
	assert.Equal(t, 1, result.Metrics.Retries)
	assert.Equal(t, int32(2), requests.Load())
}

func TestClientAcceptsWebPSource(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		assert.Contains(t, string(body), `data:image/webp;base64,`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"glm-ocr","deployment_fingerprint":"`+glmocr.DefaultDeploymentFingerprint+`","json_result":[[{"index":0,"label":"text","content":"synthetic webp"}]]}`)
	}))
	defer server.Close()
	content := append([]byte("RIFF\x04\x00\x00\x00WEBP"), []byte("VP8 ")...)
	digest := sha256.Sum256(content)
	source, err := ocr.NewSource(
		io.NopCloser(bytes.NewReader(content)), "image/webp", int64(len(content)), hex.EncodeToString(digest[:]),
	)
	require.NoError(t, err)

	result, err := newTestClient(t, server.URL+"/glmocr/parse", 1<<20, 1).Process(context.Background(), source)
	require.NoError(t, err)
	assert.Equal(t, "synthetic webp", result.Document.Units[0].Text)
}

func TestClientClassifiesBoundedAndMalformedResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
		max  int64
		kind ocr.ErrorKind
	}{
		{name: "too large", body: strings.Repeat("x", 1025), max: 1024, kind: ocr.ErrorResponseTooLarge},
		{name: "deployment drift", body: `{"model":"glm-ocr","deployment_fingerprint":"stale","json_result":[[{"index":0,"label":"text","content":"x"}]]}`, max: 1 << 20, kind: ocr.ErrorMalformedOutput},
		{name: "model drift", body: `{"model":"glm-ocr-latest","deployment_fingerprint":"` + glmocr.DefaultDeploymentFingerprint + `","json_result":[[{"index":0,"label":"text","content":"x"}]]}`, max: 1 << 20, kind: ocr.ErrorMalformedOutput},
		{name: "page index", body: `{"model":"glm-ocr","deployment_fingerprint":"` + glmocr.DefaultDeploymentFingerprint + `","json_result":[[{"index":4,"label":"text","content":"x"}]]}`, max: 1 << 20, kind: ocr.ErrorMalformedOutput},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			client := newTestClient(t, server.URL+"/glmocr/parse", test.max, 1)

			_, err := client.Process(context.Background(), syntheticPNGSource(t))
			require.Error(t, err)
			assert.Equal(t, test.kind, ocr.ErrorKindOf(err))
		})
	}
}

func TestClientCancellationClosesBlockedSource(t *testing.T) {
	reader := &blockingReadCloser{entered: make(chan struct{}), closed: make(chan struct{})}
	digest := sha256.Sum256(make([]byte, 512))
	source, err := ocr.NewSource(reader, "image/png", 512, hex.EncodeToString(digest[:]))
	require.NoError(t, err)
	client := newTestClient(t, "http://127.0.0.1:30004/glmocr/parse", 1<<20, 1)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, processErr := client.Process(ctx, source)
		done <- processErr
	}()
	<-reader.entered
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("Process did not interrupt the blocked source read")
	}
	assert.Equal(t, int32(1), reader.closeCalls.Load())
}

func TestHealthRequiresDeploymentFingerprint(t *testing.T) {
	for _, test := range []struct {
		name        string
		fingerprint string
		wantError   bool
	}{
		{name: "matching", fingerprint: glmocr.DefaultDeploymentFingerprint},
		{name: "stale", fingerprint: "stale", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"ok","deployment_fingerprint":"`+test.fingerprint+`"}`)
			}))
			defer server.Close()
			client := newTestClient(t, server.URL+"/glmocr/parse", 1<<20, 1)
			err := client.Health(t.Context())
			if test.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func newTestClient(t *testing.T, endpoint string, maxResponseBytes int64, maxUnits int) *glmocr.Client {
	t.Helper()
	normalize, err := document.NewNormalizePolicy(1_000_000)
	require.NoError(t, err)
	policy, err := glmocr.NewPolicy(glmocr.PolicyConfig{
		Endpoint: endpoint, MaxDocumentBytes: 1 << 20,
		MaxResponseBytes: maxResponseBytes, MaxUnits: maxUnits, NormalizePolicy: normalize,
	})
	require.NoError(t, err)
	client, err := glmocr.NewClient(policy, glmocr.ClientConfig{
		Timeout: 5 * time.Second, MaxRetries: 1, MaxRetryDelay: time.Millisecond,
	})
	require.NoError(t, err)
	return client
}

func syntheticPNGSource(t *testing.T) ocr.Source {
	t.Helper()
	content := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 504)...)
	digest := sha256.Sum256(content)
	source, err := ocr.NewSource(
		io.NopCloser(strings.NewReader(string(content))), "image/png", int64(len(content)),
		hex.EncodeToString(digest[:]),
	)
	require.NoError(t, err, "synthetic PNG source has %d bytes", len(content))
	return source
}

type blockingReadCloser struct {
	entered    chan struct{}
	closed     chan struct{}
	enterOnce  sync.Once
	closeOnce  sync.Once
	closeCalls atomic.Int32
}

func (r *blockingReadCloser) Read([]byte) (int, error) {
	r.enterOnce.Do(func() { close(r.entered) })
	<-r.closed
	return 0, errors.New("source closed")
}

func (r *blockingReadCloser) Close() error {
	r.closeCalls.Add(1)
	r.closeOnce.Do(func() { close(r.closed) })
	return nil
}
