package conversionworker

import (
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWorkerRetriesExhaustedSubmission(t *testing.T) {
	o := workerOptions(t)
	normal := o.Client.Transport
	var posts int
	o.Client = &http.Client{Transport: backendTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			posts++
			if posts <= 3 {
				return backendResponse(r, http.StatusServiceUnavailable, `{}`), nil
			}
		}
		return normal.RoundTrip(r)
	})}
	w := openWorker(t, o)
	key := strings.Repeat("a", 64)
	id := taskID(t, submitRequest(t, w, key, []byte("source")))
	for range 4 {
		_, err := w.RunOnce(t.Context())
		require.NoError(t, err)
	}
	require.Contains(t, getRequest(t.Context(), w, "/v1/status/poll/"+id).Body.String(), "failure")
	require.Equal(t, id, taskID(t, submitRequest(t, w, key, []byte("source"))))
	_, err := w.RunOnce(t.Context())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, getRequest(t.Context(), w, "/v1/result/"+id).Code)
}

func TestWorkerStalledTaskLetsPendingJobStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := workerOptions(t)
		o.StallAfter = time.Hour
		var posts int
		o.Client = &http.Client{Transport: backendTransport(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodPost {
				posts++
				_, _ = io.Copy(io.Discard, r.Body)
				return backendResponse(r, 202, `{"task_id":"backend-1","task_status":"pending","task_type":"convert"}`), nil
			}
			if strings.HasPrefix(r.URL.Path, "/v1/status/poll/") {
				return backendResponse(r, 200, `{"task_id":"backend-1","task_status":"started","task_type":"convert"}`), nil
			}
			return backendResponse(r, 200, `{}`), nil
		})}
		w := openWorker(t, o)
		taskID(t, submitRequest(t, w, strings.Repeat("a", 64), []byte("first")))
		_, err := w.RunOnce(t.Context())
		require.NoError(t, err)
		taskID(t, submitRequest(t, w, strings.Repeat("b", 64), []byte("second")))
		time.Sleep(time.Hour)
		_, err = w.RunOnce(t.Context())
		require.NoError(t, err)
		require.Equal(t, 2, posts)
	})
}

func TestWorkerPollsEveryStalledTask(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := workerOptions(t)
		o.StallAfter = time.Hour
		var posts int
		complete := false
		o.Client = &http.Client{Transport: backendTransport(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodPost {
				posts++
				_, _ = io.Copy(io.Discard, r.Body)
				id := "first"
				if posts == 2 {
					id = "second"
				}
				return backendResponse(r, 202, `{"task_id":"`+id+`","task_status":"pending","task_type":"convert"}`), nil
			}
			if id, ok := strings.CutPrefix(r.URL.Path, "/v1/status/poll/"); ok {
				if id == "second" && complete {
					return backendResponse(r, 200, `{"task_id":"second","task_status":"success","task_type":"convert"}`), nil
				}
				return backendResponse(r, 200, `{"task_id":"`+id+`","task_status":"started","task_type":"convert"}`), nil
			}
			if strings.HasPrefix(r.URL.Path, "/v1/result/") {
				return backendResponse(r, 200, `{"result":"complete"}`), nil
			}
			return backendResponse(r, 200, `{}`), nil
		})}
		w := openWorker(t, o)
		taskID(t, submitRequest(t, w, strings.Repeat("a", 64), []byte("first")))
		_, err := w.RunOnce(t.Context())
		require.NoError(t, err)
		second := taskID(t, submitRequest(t, w, strings.Repeat("b", 64), []byte("second")))
		time.Sleep(time.Hour)
		_, err = w.RunOnce(t.Context())
		require.NoError(t, err)
		time.Sleep(time.Hour)
		complete = true
		for range 2 {
			_, err = w.RunOnce(t.Context())
			require.NoError(t, err)
		}
		require.Equal(t, 200, getRequest(t.Context(), w, "/v1/result/"+second).Code)
		require.Equal(t, 2, posts)
	})
}

func TestWorkerCollectsStalledResultWhilePendingIsRateLimited(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := workerOptions(t)
		o.StallAfter = time.Hour
		posts := 0
		complete := false
		o.Client = &http.Client{Transport: backendTransport(func(r *http.Request) (*http.Response, error) {
			switch {
			case r.Method == http.MethodPost:
				posts++
				_, _ = io.Copy(io.Discard, r.Body)
				if posts > 1 {
					return backendResponse(r, 429, `{}`), nil
				}
				return backendResponse(r, 202, `{"task_id":"first","task_status":"pending","task_type":"convert"}`), nil
			case strings.HasPrefix(r.URL.Path, "/v1/status/poll/"):
				if complete {
					return backendResponse(r, 200, `{"task_id":"first","task_status":"success","task_type":"convert"}`), nil
				}
				return backendResponse(r, 200, `{"task_id":"first","task_status":"started","task_type":"convert"}`), nil
			case strings.HasPrefix(r.URL.Path, "/v1/result/"):
				return backendResponse(r, 200, `{"result":"complete"}`), nil
			default:
				return backendResponse(r, 200, `{}`), nil
			}
		})}
		w := openWorker(t, o)
		first := taskID(t, submitRequest(t, w, strings.Repeat("a", 64), []byte("first")))
		_, err := w.RunOnce(t.Context())
		require.NoError(t, err)
		taskID(t, submitRequest(t, w, strings.Repeat("b", 64), []byte("second")))
		time.Sleep(time.Hour)
		complete = true
		for range 2 {
			_, err = w.RunOnce(t.Context())
			require.NoError(t, err)
		}
		require.Equal(t, 200, getRequest(t.Context(), w, "/v1/result/"+first).Code)
		require.LessOrEqual(t, posts, 2)
	})
}

func TestWorkerHardRejectionDoesNotBecomeRetryable(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			o := workerOptions(t)
			var posts int
			normal := o.Client.Transport
			o.Client = &http.Client{Transport: backendTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPost {
					posts++
					if posts < 3 {
						return backendResponse(r, 503, `{}`), nil
					}
					return backendResponse(r, status, `{}`), nil
				}
				return normal.RoundTrip(r)
			})}
			w := openWorker(t, o)
			key := strings.Repeat("a", 64)
			id := taskID(t, submitRequest(t, w, key, []byte("source")))
			for range 3 {
				_, err := w.RunOnce(t.Context())
				require.NoError(t, err)
			}
			out := getRequest(t.Context(), w, "/v1/status/poll/"+id)
			require.Contains(t, out.Body.String(), "failure")
			response := submitRequest(t, w, key, []byte("source"))
			if status == 400 {
				require.Contains(t, response.Body.String(), "failure")
			} else {
				require.Contains(t, response.Body.String(), "pending")
			}
		})
	}
}

func TestWorkerPreservesAdmissionLimitsAfterReopen(t *testing.T) {
	o := workerOptions(t)
	w := openWorker(t, o)
	id := taskID(t, submitRequest(t, w, strings.Repeat("a", 64), []byte("source")))
	require.NoError(t, w.Close(t.Context()))
	o.MaxSourceBytes = 1
	o.MaxResultBytes = 1
	o.MaxSpoolBytes = 18000
	w = openWorker(t, o)
	_, err := w.RunOnce(t.Context())
	require.NoError(t, err)
	require.Equal(t, 200, getRequest(t.Context(), w, "/v1/result/"+id).Code)
	taskID(t, submitRequest(t, w, strings.Repeat("b", 64), []byte("x")))
}

func TestWorkerConfigurationAndUploadErrorsNameRequirement(t *testing.T) {
	o := workerOptions(t)
	o.MaxJobs = 129
	_, err := Open(t.Context(), o)
	require.ErrorContains(t, err, "max-jobs")
	o = workerOptions(t)
	o.APIKey = "short"
	_, err = Open(t.Context(), o)
	require.ErrorContains(t, err, "DOCBANK_WORKER_API_KEY")
	require.NotContains(t, err.Error(), "short")
	o = workerOptions(t)
	w := openWorker(t, o)
	response := submitNamed(t, w, strings.Repeat("a", 64), "notes.txt", "text/plain", []byte("hello"), [][2]string{{"to_formats", "json"}, {"target_type", "inbody"}})
	require.Equal(t, 400, response.Code)
	require.Contains(t, response.Body.String(), "supported media type")
}

func TestWorkerOpenRejectsZeroLimitsAndInvalidHeaders(t *testing.T) {
	for _, test := range []struct {
		field string
		set   func(*Options)
	}{
		{"max-source-bytes", func(o *Options) { o.MaxSourceBytes = 0 }},
		{"max-result-bytes", func(o *Options) { o.MaxResultBytes = 0 }},
		{"max-jobs", func(o *Options) { o.MaxJobs = 0 }},
		{"max-spool-bytes", func(o *Options) { o.MaxSpoolBytes = 0 }},
		{"DOCBANK_WORKER_API_KEY", func(o *Options) { o.APIKey = "synthetic-invalid\nkey" }},
		{"DOCBANK_WORKER_BACKEND_KEY", func(o *Options) { o.BackendKey = "synthetic-invalid\nkey" }},
	} {
		t.Run(test.field, func(t *testing.T) {
			o := workerOptions(t)
			test.set(&o)
			_, err := Open(t.Context(), o)
			require.ErrorContains(t, err, test.field)
			require.NotContains(t, err.Error(), "synthetic-invalid")
			_, err = os.Stat(o.Directory)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestWorkerKeepsJobsAfterLimitsLowered(t *testing.T) {
	o := workerOptions(t)
	w := openWorker(t, o)
	key := strings.Repeat("a", 64)
	id := taskID(t, submitRequest(t, w, key, []byte("source")))
	_, err := w.RunOnce(t.Context())
	require.NoError(t, err)
	taskID(t, submitRequest(t, w, strings.Repeat("b", 64), []byte("other")))
	require.NoError(t, w.Close(t.Context()))
	o.MaxSourceBytes = 1
	o.MaxResultBytes = 1
	o.MaxJobs = 1
	o.MaxSpoolBytes = overhead
	w = openWorker(t, o)
	require.Equal(t, id, taskID(t, submitRequest(t, w, key, []byte("source"))))
	require.Equal(t, 200, getRequest(t.Context(), w, "/v1/result/"+id).Code)
	require.Equal(t, 429, submitRequest(t, w, strings.Repeat("c", 64), []byte("x")).Code)
}

func TestWorkerRejectsUnknownManifestVersion(t *testing.T) {
	o := workerOptions(t)
	w := openWorker(t, o)
	id := taskID(t, submitRequest(t, w, strings.Repeat("a", 64), []byte("source")))
	item := w.jobs[id]
	item.Version = 2
	require.NoError(t, w.save(t.Context(), item))
	require.NoError(t, w.Close(t.Context()))
	_, err := Open(t.Context(), o)
	require.Error(t, err)
}

func TestWorkerRejectsWrongBackendTaskType(t *testing.T) {
	o := workerOptions(t)
	normal := o.Client.Transport
	var results int
	o.Client = &http.Client{Transport: backendTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Path, "/v1/status/poll/") {
			return backendResponse(r, 200, `{"task_id":"backend-1","task_status":"success","task_type":"chunk"}`), nil
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
	require.Contains(t, getRequest(t.Context(), w, "/v1/status/poll/"+id).Body.String(), "failure")
	require.Zero(t, results)
}

func TestWorkerAudioDurationLimit(t *testing.T) {
	o := workerOptions(t)
	o.MaxSourceBytes = 32 << 20
	o.MaxSpoolBytes = 128 << 20
	w := openWorker(t, o)
	fields := [][2]string{{"from_formats", "audio"}, {"to_formats", "json"}, {"target_type", "inbody"}}
	for i, test := range []struct {
		seconds int
		status  int
	}{{3600, 202}, {3601, 400}} {
		data := make([]byte, 44+8000*test.seconds)
		copy(data, "RIFF")
		binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
		copy(data[8:], "WAVEfmt ")
		binary.LittleEndian.PutUint32(data[16:20], 16)
		binary.LittleEndian.PutUint16(data[20:22], 1)
		binary.LittleEndian.PutUint16(data[22:24], 1)
		binary.LittleEndian.PutUint32(data[24:28], 8000)
		binary.LittleEndian.PutUint32(data[28:32], 8000)
		binary.LittleEndian.PutUint16(data[32:34], 1)
		binary.LittleEndian.PutUint16(data[34:36], 8)
		copy(data[36:], "data")
		binary.LittleEndian.PutUint32(data[40:44], uint32(len(data)-44))
		response := submitNamed(t, w, strings.Repeat(string(rune('a'+i)), 64), "audio.wav", "audio/wav", data, fields)
		require.Equal(t, test.status, response.Code, response.Body.String())
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
			return backendResponse(r, 202, `{"task_id":"new-task","task_status":"pending","task_type":"convert"}`), nil
		}
		if id, ok := strings.CutPrefix(r.URL.Path, "/v1/status/poll/"); ok {
			state := "started"
			if id == "accepted-task" && complete.Load() {
				state = "success"
			}
			return backendResponse(r, 200, `{"task_id":"`+id+`","task_status":"`+state+`","task_type":"convert"}`), nil
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
	item.StartedAt = time.Now()
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
