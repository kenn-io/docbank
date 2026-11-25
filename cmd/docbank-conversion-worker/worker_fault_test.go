package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWorkerSchedulesStalledAndPendingJobs(t *testing.T) {
	t.Run("submits pending and collects stalled after rate limit", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			o := workerOptions(t)
			o.StallAfter = time.Hour
			posts := 0
			complete := false
			o.Client = &http.Client{Transport: backendTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPost {
					posts++
					_, _ = io.Copy(io.Discard, r.Body)
					if posts == 1 {
						return backendResponse(r, http.StatusTooManyRequests, `{}`), nil
					}
					return backendResponse(r, 202, `{"task_id":"new-task","task_status":"pending","task_type":"convert"}`), nil
				}
				if id, ok := strings.CutPrefix(r.URL.Path, "/v1/status/poll/"); ok {
					status := "started"
					if complete || id == "new-task" {
						status = "success"
					}
					return backendResponse(r, 200, `{"task_id":"`+id+`","task_status":"`+status+`","task_type":"convert"}`), nil
				}
				return backendResponse(r, 200, `{"status":"success"}`), nil
			})}
			w := openWorker(t, o)
			first := taskID(t, submitRequest(t, w, strings.Repeat("a", 64), []byte("first")))
			second := taskID(t, submitRequest(t, w, strings.Repeat("b", 64), []byte("second")))
			active, pending := max(first, second), min(first, second)
			item := w.jobs[active]
			pendingItem := w.jobs[pending]
			item.State, item.BackendID, item.Submissions, item.StartedAt = "started", "accepted-task", 1, time.Now()
			require.NoError(t, w.save(t.Context(), item))
			require.NoError(t, w.Close())
			w = openWorker(t, o)
			_, err := w.RunOnce(t.Context())
			require.NoError(t, err)
			require.Zero(t, posts, "a fresh task holds pending work")
			time.Sleep(time.Hour)
			_, err = w.RunOnce(t.Context())
			require.NoError(t, err)
			require.Equal(t, 1, posts, "pending work receives a rate limit")
			complete = true
			_, err = w.RunOnce(t.Context())
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, getRequest(t.Context(), w, "/v1/result/"+active).Code)
			_, err = w.RunOnce(t.Context())
			require.NoError(t, err)
			require.Equal(t, 2, posts)
			require.Equal(t, http.StatusOK, getRequest(t.Context(), w, "/v1/result/"+pending).Code)

			item.StartedAt = time.Now()
			require.NoError(t, w.save(t.Context(), item))
			require.NoError(t, w.save(t.Context(), pendingItem))
			complete = false
			_, err = w.RunOnce(t.Context())
			require.NoError(t, err)
			require.Equal(t, 2, posts, "a fresh task holds pending work")
			complete = true
			_, err = w.RunOnce(t.Context())
			require.NoError(t, err)
			require.Equal(t, 2, posts, "completion must precede pending submission")
			_, err = w.RunOnce(t.Context())
			require.NoError(t, err)
			require.Equal(t, 3, posts, "completion releases the hold before the stall limit")
		})
	})
}

func TestWorkerOpenRejectsInvalidKeys(t *testing.T) {
	for _, test := range []struct {
		field string
		set   func(*Options)
	}{
		{"DOCBANK_WORKER_API_KEY short", func(o *Options) { o.APIKey = "short" }},
		{"DOCBANK_WORKER_API_KEY", func(o *Options) { o.APIKey = "synthetic-invalid\nkey" }},
		{"DOCBANK_WORKER_BACKEND_KEY", func(o *Options) { o.BackendKey = "synthetic-invalid\nkey" }},
	} {
		t.Run(test.field, func(t *testing.T) {
			o := workerOptions(t)
			test.set(&o)
			_, err := Open(t.Context(), o)
			require.ErrorContains(t, err, strings.TrimSuffix(test.field, " short"))
			require.NotContains(t, err.Error(), "synthetic-invalid")
			require.NotContains(t, err.Error(), "short")
			_, err = os.Stat(o.Directory)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestWorkerRejectsWrongBackendTaskType(t *testing.T) {
	for _, stage := range []string{"submit", "poll"} {
		t.Run(stage, func(t *testing.T) {
			o := workerOptions(t)
			normal := o.Client.Transport
			var polls, results int
			o.Client = &http.Client{Transport: backendTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPost && stage == "submit" {
					_, _ = io.Copy(io.Discard, r.Body)
					return backendResponse(r, 202, `{"task_id":"backend-1","task_status":"pending","task_type":"chunk"}`), nil
				}
				if strings.HasPrefix(r.URL.Path, "/v1/status/poll/") {
					polls++
					if stage == "poll" {
						return backendResponse(r, 200, `{"task_id":"backend-1","task_status":"success","task_type":"chunk"}`), nil
					}
				}
				if strings.HasPrefix(r.URL.Path, "/v1/result/") {
					results++
				}
				return normal.RoundTrip(r)
			})}
			w := openWorker(t, o)
			id := taskID(t, submitRequest(t, w, strings.Repeat("a", 64), []byte("source")))
			_, err := w.RunOnce(t.Context())
			require.NoError(t, err)
			if stage == "submit" {
				require.Zero(t, polls)
				require.Contains(t, getRequest(t.Context(), w, "/v1/status/poll/"+id).Body.String(), `"task_status":"pending"`)
			} else {
				require.Contains(t, getRequest(t.Context(), w, "/v1/status/poll/"+id).Body.String(), "failure")
			}
			require.Zero(t, results)
		})
	}
}

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
	require.NoError(t, w.Close())
	<-done
	require.NoError(t, writer.Close())
	next, err := Open(t.Context(), o)
	require.NoError(t, err)
	require.NoError(t, next.Close())
}
func TestWorkerDoesNotSubmitCorruptedRetainedSourceUnderStableKey(t *testing.T) {
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
	key := strings.Repeat("a", 64)
	id := taskID(t, submitRequest(t, w, key, []byte("original")))
	require.NoError(t, os.WriteFile(filepath.Join(o.Directory, id, "source"), []byte("tampered"), 0600))
	_, err := w.RunOnce(t.Context())
	require.NoError(t, err)
	require.Zero(t, submitted, "damaged source bytes must never be posted for the retained idempotency key")
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
	require.NoError(t, w.Close())
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
		require.NoError(t, w.Close())
		next, err := Open(t.Context(), o)
		require.NoError(t, err)
		require.NoError(t, next.Close())
	})
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
	_, _, err := w.backendRequest(t.Context(), http.MethodGet, "/ready", nil, "", w.options.MaxResultBytes)
	require.NoError(t, err)
}

func TestWorkerSubmissionBudget(t *testing.T) {
	for _, scenario := range []string{"503 response", "lost response", "400 rejection", "400 body error", "401 rejection"} {
		t.Run(scenario, func(t *testing.T) {
			o := workerOptions(t)
			if scenario == "401 rejection" {
				o.BackendKey = "synthetic-old-backend-key"
			}
			normal := o.Client.Transport
			posts := 0
			var submittedKeys []string
			o.Client = &http.Client{Transport: backendTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPost {
					_, _ = io.Copy(io.Discard, r.Body)
					posts++
					if scenario == "401 rejection" {
						submittedKeys = append(submittedKeys, r.Header.Get("X-Api-Key"))
					}
					if posts <= 3 {
						if scenario == "lost response" {
							return nil, errors.New("synthetic lost response")
						}
						if posts == 3 {
							if scenario == "401 rejection" {
								return backendResponse(r, http.StatusUnauthorized, `{}`), nil
							}
							if scenario == "400 body error" {
								return &http.Response{Request: r, StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: backendResponseReadFailure{err: errors.New("synthetic body read failure")}}, nil
							}
							if strings.HasPrefix(scenario, "400 ") {
								return backendResponse(r, http.StatusBadRequest, `{}`), nil
							}
						}
						return backendResponse(r, 503, `{}`), nil
					}
				}
				return normal.RoundTrip(r)
			})}
			w := openWorker(t, o)
			key := strings.Repeat("a", 64)
			id := taskID(t, submitRequest(t, w, key, []byte("source")))
			for range 3 {
				_, err := w.RunOnce(t.Context())
				require.NoError(t, err)
				if scenario == "lost response" {
					require.NoError(t, w.Close())
					w = openWorker(t, o)
				}
			}
			_, err := w.RunOnce(t.Context())
			require.NoError(t, err)
			require.Equal(t, 3, posts)
			require.Contains(t, getRequest(t.Context(), w, "/v1/status/poll/"+id).Body.String(), "failure")
			require.NoError(t, w.Close())
			if scenario == "401 rejection" {
				o.BackendKey = "synthetic-corrected-backend-key"
			}
			w = openWorker(t, o)
			response := submitRequest(t, w, key, []byte("source"))
			if strings.HasPrefix(scenario, "400 ") {
				require.Contains(t, response.Body.String(), "failure")
				return
			}
			require.Equal(t, id, taskID(t, response))
			_, err = w.RunOnce(t.Context())
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, getRequest(t.Context(), w, "/v1/result/"+id).Code)
			if scenario == "401 rejection" {
				require.Equal(t, []string{"synthetic-old-backend-key", "synthetic-old-backend-key", "synthetic-old-backend-key", "synthetic-corrected-backend-key"}, submittedKeys)
			}
		})
	}
}

func TestWorkerReopensAfterLimitsLowered(t *testing.T) {
	for _, overCapacity := range []bool{false, true} {
		t.Run(map[bool]string{false: "lowered source/result", true: "lowered jobs/spool"}[overCapacity], func(t *testing.T) {
			o := workerOptions(t)
			w := openWorker(t, o)
			key := strings.Repeat("a", 64)
			id := taskID(t, submitRequest(t, w, key, []byte("source")))
			if overCapacity {
				_, err := w.RunOnce(t.Context())
				require.NoError(t, err)
				taskID(t, submitRequest(t, w, strings.Repeat("b", 64), []byte("other")))
			}
			require.NoError(t, w.Close())
			o.MaxSourceBytes, o.MaxResultBytes = 1, 1
			if overCapacity {
				o.MaxJobs, o.MaxSpoolBytes = 1, overhead
			} else {
				o.MaxSpoolBytes = 18000
			}
			w = openWorker(t, o)
			if !overCapacity {
				_, err := w.RunOnce(t.Context())
				require.NoError(t, err)
			}
			require.Equal(t, id, taskID(t, submitRequest(t, w, key, []byte("source"))))
			require.Equal(t, 200, getRequest(t.Context(), w, "/v1/result/"+id).Code)
			if !overCapacity {
				taskID(t, submitRequest(t, w, strings.Repeat("c", 64), []byte("x")))
			}
		})
	}
}
