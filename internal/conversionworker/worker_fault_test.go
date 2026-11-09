package conversionworker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
)

func TestWorkerCloseCancelsUploadBeforeUnlock(t *testing.T) {
	o := workerOptions(t)
	w := openWorker(t, o)
	reader, writer := io.Pipe()
	bodyStarted := make(chan struct{})
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/convert/file/async", reader)
	request.Header.Set("Content-Type", "multipart/form-data; boundary=synthetic")
	request.Header.Set("X-Api-Key", o.APIKey)
	request.Header.Set("Idempotency-Key", strings.Repeat("a", 64))
	request.Header.Set("X-Docbank-Source-Sha256", strings.Repeat("b", 64))
	done := make(chan struct{})
	go func() { defer close(done); w.ServeHTTP(httptest.NewRecorder(), request) }()
	go func() {
		_, _ = io.WriteString(writer, "--synthetic\r\nContent-Disposition: form-data; name=\"files\"; filename=\"synthetic.pdf\"\r\nContent-Type: application/pdf\r\n\r\n")
		close(bodyStarted)
	}()
	<-bodyStarted
	require.NoError(t, w.Close(t.Context()))
	<-done
	require.NoError(t, writer.Close())
	next, err := Open(t.Context(), o)
	require.NoError(t, err)
	require.NoError(t, next.Close(t.Context()))
}
func TestWorkerRejectsDamagedRetainedSource(t *testing.T) {
	o := workerOptions(t)
	w := openWorker(t, o)
	id := taskID(t, submitRequest(t, w, strings.Repeat("a", 64), []byte("original")))
	require.NoError(t, w.Close(t.Context()))
	require.NoError(t, os.WriteFile(filepath.Join(o.Directory, id, "source"), []byte("tampered"), 0600))
	next, err := Open(t.Context(), o)
	require.Error(t, err)
	require.Nil(t, next)
}

func TestWorkerDoesNotSubmitCorruptedRetainedSourceUnderStableKey(t *testing.T) {
	for _, test := range []struct {
		name   string
		damage []byte
	}{
		{name: "same-length corruption", damage: []byte("tampered")},
		{name: "truncated source", damage: []byte("short")},
	} {
		t.Run(test.name, func(t *testing.T) {
			o := workerOptions(t)
			normal := o.Client.Transport
			var submitted int
			o.Client = &http.Client{Transport: backendTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPost {
					submitted++
					_, _ = io.Copy(io.Discard, r.Body)
				}
				return normal.RoundTrip(r)
			})}
			w := openWorker(t, o)
			const key = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			id := taskID(t, submitRequest(t, w, key, []byte("original")))
			require.Equal(t, id, w.keys[key])
			require.NoError(t, os.WriteFile(filepath.Join(o.Directory, id, "source"), test.damage, 0600))
			_, err := w.RunOnce(t.Context())
			require.NoError(t, err)
			require.Zero(t, submitted, "damaged source bytes must never be posted for the retained idempotency key")
			require.Equal(t, id, w.keys[key])
		})
	}
}

func TestWorkerOversizeResultIsTerminal(t *testing.T) {
	o := workerOptions(t)
	normal := o.Client.Transport
	result := `"` + strings.Repeat("x", 4095) + `"`
	require.Len(t, result, 4097)
	o.Client = &http.Client{Transport: backendTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Path, "/v1/result/") {
			return backendResponse(r, 200, result), nil
		}
		return normal.RoundTrip(r)
	})}
	w := openWorker(t, o)
	id := taskID(t, submitRequest(t, w, strings.Repeat("a", 64), []byte("source")))
	_, err := w.RunOnce(t.Context())
	require.NoError(t, err)
	require.Contains(t, getRequest(t.Context(), w, "/v1/status/poll/"+id).Body.String(), "failure")
}

func TestWorkerDoesNotSubmitAfterFailedManifestCommit(t *testing.T) {
	o := workerOptions(t)
	var submissions atomic.Int32
	normal := o.Client.Transport
	o.Client = &http.Client{Transport: backendTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			submissions.Add(1)
		}
		return normal.RoundTrip(r)
	})}
	w := openWorker(t, o)
	id := taskID(t, submitRequest(t, w, strings.Repeat("a", 64), []byte("source")))
	manifest := filepath.Join(o.Directory, id, "manifest.json")
	require.NoError(t, os.Remove(manifest))
	require.NoError(t, os.Mkdir(manifest, 0700))
	_, err := w.RunOnce(t.Context())
	require.Error(t, err)
	require.Zero(t, submissions.Load())
	require.Equal(t, 503, getRequest(t.Context(), w, "/ready").Code)
	require.Equal(t, 503, submitRequest(t, w, strings.Repeat("b", 64), []byte("synthetic retry")).Code)
}
func TestWorkerRecoversLostBackendTask(t *testing.T) {
	o := workerOptions(t)
	var submissions atomic.Int32
	var polls atomic.Int32
	normal := o.Client.Transport
	o.Client = &http.Client{Transport: backendTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			submissions.Add(1)
		}
		if strings.HasPrefix(r.URL.Path, "/v1/status/poll/") && polls.Add(1) == 1 {
			return backendResponse(r, 404, `{}`), nil
		}
		return normal.RoundTrip(r)
	})}
	w := openWorker(t, o)
	id := taskID(t, submitRequest(t, w, strings.Repeat("a", 64), []byte("source")))
	_, err := w.RunOnce(t.Context())
	require.NoError(t, err)
	require.NoError(t, w.Close(t.Context()))
	w = openWorker(t, o)
	_, err = w.RunOnce(t.Context())
	require.NoError(t, err)
	require.Equal(t, int32(2), submissions.Load())
	require.Equal(t, 200, getRequest(t.Context(), w, "/v1/result/"+id).Code)
}

func TestWorkerRunStopsAndReleasesOwnership(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := workerOptions(t)
		w := openWorker(t, o)
		id := taskID(t, submitRequest(t, w, strings.Repeat("a", 64), []byte("source")))
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- w.Run(ctx) }()
		synctest.Wait()
		require.Equal(t, 200, getRequest(t.Context(), w, "/v1/result/"+id).Code)
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
		require.NoError(t, w.Close(t.Context()))
		next, err := Open(t.Context(), o)
		require.NoError(t, err)
		require.NoError(t, next.Close(t.Context()))
	})
}

func TestWorkerFinishesKnownBackendTaskBeforeLowerIDPendingJob(t *testing.T) {
	o := workerOptions(t)
	var posts atomic.Int32
	var complete atomic.Bool
	var w *Worker
	var active string
	retainedAtSubmit := false
	o.Client = &http.Client{Transport: backendTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			retainedAtSubmit = getRequest(t.Context(), w, "/v1/result/"+active).Code == 200
			_, _ = io.Copy(io.Discard, r.Body)
			return backendResponse(r, 202, `{"task_id":"new-task","task_status":"pending"}`), nil
		}
		if id, ok := strings.CutPrefix(r.URL.Path, "/v1/status/poll/"); ok {
			state := "started"
			if id == "accepted-task" && complete.Load() {
				state = "success"
			}
			return backendResponse(r, 200, `{"task_id":"`+id+`","task_status":"`+state+`"}`), nil
		}
		return backendResponse(r, 200, `{"status":"success"}`), nil
	})}
	w = openWorker(t, o)
	first := taskID(t, submitRequest(t, w, strings.Repeat("a", 64), []byte("first")))
	second := taskID(t, submitRequest(t, w, strings.Repeat("b", 64), []byte("second")))
	// Restore a valid known-handle state on the larger random issued ID. The
	// lower ID remains pending, making the scheduler assertion deterministic.
	active = max(first, second)
	item := w.jobs[active]
	item.State = "started"
	item.BackendID = "accepted-task"
	item.Submissions = 1
	require.NoError(t, w.save(t.Context(), item))
	require.NoError(t, w.Close(t.Context()))
	w = openWorker(t, o)
	worked, err := w.RunOnce(t.Context())
	require.NoError(t, err)
	require.True(t, worked)
	require.Zero(t, posts.Load(), "new work must wait while the accepted backend task remains started")
	complete.Store(true)
	_, err = w.RunOnce(t.Context())
	require.NoError(t, err)
	require.Zero(t, posts.Load())
	require.Equal(t, 200, getRequest(t.Context(), w, "/v1/result/"+active).Code)
	_, err = w.RunOnce(t.Context())
	require.NoError(t, err)
	require.Equal(t, int32(1), posts.Load())
	require.True(t, retainedAtSubmit)
}

func TestWorkerRejectsBackendHopBeforeReadinessEgress(t *testing.T) {
	o := workerOptions(t)
	calls := 0
	o.Client = &http.Client{Transport: backendTransport(func(r *http.Request) (*http.Response, error) { calls++; return backendResponse(r, 200, `{}`), nil })}
	w := openWorker(t, o)
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ready", nil)
	r.Header.Set("X-Api-Key", o.APIKey)
	r.Header.Set("X-Docbank-Worker-Backend-Hop", "1")
	out := httptest.NewRecorder()
	w.ServeHTTP(out, r)
	require.Equal(t, 400, out.Code)
	require.Zero(t, calls, "gateway chains must stop before making another readiness request")
}
func TestWorkerBackendRequestsCarryHopMarker(t *testing.T) {
	o := workerOptions(t)
	o.Client = &http.Client{Transport: backendTransport(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "1", r.Header.Get("X-Docbank-Worker-Backend-Hop"))
		return backendResponse(r, 200, `{}`), nil
	})}
	w := openWorker(t, o)
	_, _, err := w.backendRequest(t.Context(), http.MethodGet, "/ready", nil, "")
	require.NoError(t, err)
}
