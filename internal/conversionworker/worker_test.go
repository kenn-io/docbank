package conversionworker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

type backendTransport func(*http.Request) (*http.Response, error)

func (f backendTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func backendResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{Request: r, StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}
func workerOptions(t *testing.T) Options {
	t.Helper()
	t.Setenv("DOCBANK_LOCK_DIR", filepath.Join(t.TempDir(), "locks"))
	return Options{Directory: filepath.Join(t.TempDir(), "spool"), APIKey: "synthetic-worker-key", BackendURL: "http://127.0.0.1:5001", BackendKey: "synthetic-backend-key", MaxSourceBytes: 1024, MaxResultBytes: 4096, MaxJobs: 2, MaxSpoolBytes: 65536, PollInterval: time.Millisecond, Client: &http.Client{Transport: backendTransport(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == "/ready":
			return backendResponse(r, 200, `{"status":"ok"}`), nil
		case r.Method == http.MethodPost:
			_, _ = io.Copy(io.Discard, r.Body)
			return backendResponse(r, 202, `{"task_id":"backend-1","task_status":"pending"}`), nil
		case strings.HasPrefix(r.URL.Path, "/v1/status/poll/"):
			return backendResponse(r, 200, `{"task_id":"backend-1","task_status":"success"}`), nil
		case strings.HasPrefix(r.URL.Path, "/v1/result/"):
			return backendResponse(r, 200, `{"document":{"filename":"synthetic.pdf"},"status":"success"}`), nil
		}
		return backendResponse(r, 404, `{}`), nil
	})}}
}
func openWorker(t *testing.T, o Options) *Worker {
	t.Helper()
	w, err := Open(t.Context(), o)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, w.Close(context.WithoutCancel(t.Context()))) })
	return w
}
func submitRequest(t *testing.T, w *Worker, key string, source []byte) *httptest.ResponseRecorder {
	t.Helper()
	return submitNamed(t, w, key, "synthetic.pdf", "application/pdf", source, [][2]string{{"to_formats", "md"}, {"to_formats", "json"}, {"target_type", "inbody"}})
}
func submitNamed(t *testing.T, w *Worker, key, filename, mediaType string, source []byte, fields [][2]string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	m := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", multipart.FileContentDisposition("files", filename))
	header.Set("Content-Type", mediaType)
	f, err := m.CreatePart(header)
	require.NoError(t, err)
	_, err = f.Write(source)
	require.NoError(t, err)
	for _, field := range fields {
		require.NoError(t, m.WriteField(field[0], field[1]))
	}
	require.NoError(t, m.Close())
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/convert/file/async", &body)
	req.Header.Set("Content-Type", m.FormDataContentType())
	req.Header.Set("X-Api-Key", "synthetic-worker-key")
	req.Header.Set("Idempotency-Key", key)
	sum := sha256.Sum256(source)
	req.Header.Set("X-Docbank-Source-Sha256", hex.EncodeToString(sum[:]))
	out := httptest.NewRecorder()
	w.ServeHTTP(out, req)
	return out
}
func getRequest(ctx context.Context, w *Worker, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	r.Header.Set("X-Api-Key", "synthetic-worker-key")
	o := httptest.NewRecorder()
	w.ServeHTTP(o, r)
	return o
}
func taskID(t *testing.T, r *httptest.ResponseRecorder) string {
	t.Helper()
	require.Equal(t, http.StatusAccepted, r.Code, r.Body.String())
	var task struct {
		ID string `json:"task_id"`
	}
	require.NoError(t, json.Unmarshal(r.Body.Bytes(), &task))
	require.Len(t, task.ID, 32)
	return task.ID
}
func TestWorkerDurableAcceptanceAndLookup(t *testing.T) {
	o := workerOptions(t)
	w := openWorker(t, o)
	key := strings.Repeat("a", 64)
	id := taskID(t, submitRequest(t, w, key, []byte("synthetic")))
	require.NoError(t, w.Close(t.Context()))
	w = openWorker(t, o)
	found := getRequest(t.Context(), w, "/v1/jobs/by-key/"+key)
	require.Equal(t, 200, found.Code)
	require.Contains(t, found.Body.String(), id)
	require.Equal(t, id, taskID(t, submitRequest(t, w, key, []byte("synthetic"))))
	require.Equal(t, 409, submitRequest(t, w, key, []byte("changed")).Code)
}
func TestWorkerBoundsAndAuthentication(t *testing.T) {
	o := workerOptions(t)
	o.MaxJobs = 1
	w := openWorker(t, o)
	unauth := httptest.NewRecorder()
	w.ServeHTTP(unauth, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ready", nil))
	require.Equal(t, 401, unauth.Code)
	require.Equal(t, 413, submitRequest(t, w, strings.Repeat("a", 64), bytes.Repeat([]byte{1}, 1025)).Code)
	taskID(t, submitRequest(t, w, strings.Repeat("a", 64), []byte("one")))
	require.Equal(t, 429, submitRequest(t, w, strings.Repeat("b", 64), []byte("two")).Code)
	require.Equal(t, 200, getRequest(t.Context(), w, "/v1/jobs/by-key/"+strings.Repeat("a", 64)).Code)
	require.Equal(t, 202, submitRequest(t, w, strings.Repeat("a", 64), []byte("one")).Code)
}

func TestWorkerRetainsRepeatableResultsAfterReopen(t *testing.T) {
	o := workerOptions(t)
	w := openWorker(t, o)
	id := taskID(t, submitRequest(t, w, strings.Repeat("a", 64), []byte("original")))
	for range 3 {
		_, err := w.RunOnce(t.Context())
		require.NoError(t, err)
	}
	first := getRequest(t.Context(), w, "/v1/result/"+id)
	require.Equal(t, 200, first.Code, first.Body.String())
	require.NoError(t, w.Close(t.Context()))
	w = openWorker(t, o)
	for range 2 {
		out := getRequest(t.Context(), w, "/v1/result/"+id)
		require.Equal(t, 200, out.Code)
		require.Equal(t, first.Body.String(), out.Body.String())
	}
}

func TestWorkerRetainsBackendPartialSuccessAfterReopen(t *testing.T) {
	o := workerOptions(t)
	const result = `{"document":{"filename":"synthetic.pdf"},"status":"success"}`
	normal := o.Client.Transport
	o.Client = &http.Client{Transport: backendTransport(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/status/poll/"):
			return backendResponse(r, http.StatusOK, `{"task_id":"backend-1","task_status":"partial_success"}`), nil
		case strings.HasPrefix(r.URL.Path, "/v1/result/"):
			return backendResponse(r, http.StatusOK, result), nil
		default:
			return normal.RoundTrip(r)
		}
	})}
	w := openWorker(t, o)
	id := taskID(t, submitRequest(t, w, strings.Repeat("a", 64), []byte("synthetic")))
	_, err := w.RunOnce(t.Context())
	require.NoError(t, err)
	assertPartialCompletion := func(w *Worker) {
		statusResponse := getRequest(t.Context(), w, "/v1/status/poll/"+id)
		require.Equal(t, http.StatusOK, statusResponse.Code)
		var status struct {
			TaskStatus        string `json:"task_status"`
			BackendTaskStatus string `json:"backend_task_status"`
		}
		require.NoError(t, json.Unmarshal(statusResponse.Body.Bytes(), &status))
		require.Equal(t, "success", status.TaskStatus)
		require.Equal(t, "partial_success", status.BackendTaskStatus)
		resultResponse := getRequest(t.Context(), w, "/v1/result/"+id)
		require.Equal(t, http.StatusOK, resultResponse.Code, resultResponse.Body.String())
		require.JSONEq(t, result, resultResponse.Body.String())
		require.True(t, bytes.Equal([]byte(result), resultResponse.Body.Bytes()), "retained backend result bytes must be served unchanged")
	}
	assertPartialCompletion(w)
	require.NoError(t, w.Close(t.Context()))
	w = openWorker(t, o)
	assertPartialCompletion(w)
}

func TestWorkerSubmissionBudgetSurvivesRestart(t *testing.T) {
	o := workerOptions(t)
	var sends atomic.Int32
	o.Client = &http.Client{Transport: backendTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			_, _ = io.Copy(io.Discard, r.Body)
			sends.Add(1)
			return nil, errors.New("synthetic lost response")
		}
		return backendResponse(r, 200, `{"status":"ok"}`), nil
	})}
	w := openWorker(t, o)
	id := taskID(t, submitRequest(t, w, strings.Repeat("a", 64), []byte("original")))
	for range 8 {
		_, err := w.RunOnce(t.Context())
		require.NoError(t, err)
		require.NoError(t, w.Close(t.Context()))
		w = openWorker(t, o)
	}
	require.Equal(t, int32(3), sends.Load())
	out := getRequest(t.Context(), w, "/v1/status/poll/"+id)
	require.Contains(t, out.Body.String(), "failure")
}
func TestWorkerCapacityRefusalDoesNotSpendAmbiguityBudget(t *testing.T) {
	o := workerOptions(t)
	var sends atomic.Int32
	normal := o.Client.Transport
	o.Client = &http.Client{Transport: backendTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost && sends.Add(1) <= 5 {
			_, _ = io.Copy(io.Discard, r.Body)
			return backendResponse(r, 429, `{}`), nil
		}
		return normal.RoundTrip(r)
	})}
	w := openWorker(t, o)
	id := taskID(t, submitRequest(t, w, strings.Repeat("a", 64), []byte("original")))
	for range 10 {
		_, err := w.RunOnce(t.Context())
		require.NoError(t, err)
	}
	require.Equal(t, 200, getRequest(t.Context(), w, "/v1/result/"+id).Code)
	require.Equal(t, int32(6), sends.Load())
}
func TestWorkerLocksSpoolAliases(t *testing.T) {
	o := workerOptions(t)
	w := openWorker(t, o)
	second, err := Open(t.Context(), o)
	require.Error(t, err)
	require.Nil(t, second)
	require.NoError(t, w.Close(t.Context()))
	w = openWorker(t, o)
	require.NotNil(t, w)
}
func TestWorkerDuplicateIsStableForGeneratedBytes(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		source := hegel.Draw(ht, hegel.Lists(hegel.Integers[byte](0, 255)))
		source = source[:min(len(source), 512)]

		o := workerOptions(t)
		w, err := Open(t.Context(), o)
		require.NoError(ht, err)
		key := strings.Repeat("b", 64)
		first := submitRequest(t, w, key, source)
		if len(source) == 0 {
			require.Equal(ht, 400, first.Code)
			require.NoError(ht, w.Close(t.Context()))
			return
		}
		require.Equal(ht, 202, first.Code)
		require.NoError(ht, w.Close(t.Context()))
		w, err = Open(t.Context(), o)
		require.NoError(ht, err)
		second := submitRequest(t, w, key, source)
		require.Equal(ht, first.Body.String(), second.Body.String())
		require.NoError(ht, w.Close(t.Context()))
	})
}
