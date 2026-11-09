package conversionworker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type faultingBackendSource struct{ err error }

type backendResponseReadFailure struct{ err error }

func (b backendResponseReadFailure) Read([]byte) (int, error) { return 0, b.err }
func (backendResponseReadFailure) Close() error               { return nil }

type cancelingBackendResponseBody struct{ cancel context.CancelFunc }

func (b cancelingBackendResponseBody) Read([]byte) (int, error) {
	b.cancel()
	return 0, errors.New("synthetic response body read failure")
}
func (cancelingBackendResponseBody) Close() error { return nil }

func (r faultingBackendSource) Read(p []byte) (int, error) {
	return copy(p, "source-prefix"), r.err
}

func TestBackendRejectionDoesNotMaskRetainedSourceReadFailure(t *testing.T) {
	fault := errors.New("synthetic retained source read fault")
	for _, test := range []struct {
		name   string
		source io.Reader
		drain  bool
		want   error
	}{
		{"source fault", faultingBackendSource{fault}, true, fault},
		{"complete rejected input", strings.NewReader("source"), true, nil},
		{"early definitive rejection", strings.NewReader("source"), false, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := workerOptions(t)
			options.Client = &http.Client{Transport: backendTransport(func(r *http.Request) (*http.Response, error) {
				if test.drain {
					_, _ = io.Copy(io.Discard, r.Body)
				}
				return backendResponse(r, 400, `{}`), nil
			})}
			w := openWorker(t, options)
			item := job{Filename: "synthetic.pdf", MediaType: "application/pdf", Bytes: 6, Fields: [][2]string{{"to_formats", "json"}, {"target_type", "inbody"}}}
			if test.want != nil {
				item.Bytes = 1024
			}
			status, _, err := w.submitBackendStream(t.Context(), item, test.source)
			require.Equal(t, 400, status)
			if test.want != nil {
				require.ErrorIs(t, err, test.want)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestWorkerRefundsConfirmed429WhenResponseBodyReadFails(t *testing.T) {
	o := workerOptions(t)
	normal := o.Client.Transport
	posts := 0
	o.Client = &http.Client{Transport: backendTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			posts++
			if posts <= 3 {
				return &http.Response{
					Request:    r,
					StatusCode: http.StatusTooManyRequests,
					Header:     make(http.Header),
					Body:       backendResponseReadFailure{err: errors.New("synthetic backend refusal body read failure")},
				}, nil
			}
		}
		return normal.RoundTrip(r)
	})}
	w := openWorker(t, o)
	id := taskID(t, submitRequest(t, w, strings.Repeat("a", 64), []byte("synthetic")))
	for range 4 {
		_, err := w.RunOnce(t.Context())
		require.NoError(t, err)
	}
	require.Equal(t, 4, posts)
	status := getRequest(t.Context(), w, "/v1/status/poll/"+id)
	require.Equal(t, http.StatusOK, status.Code)
	require.Contains(t, status.Body.String(), `"task_status":"success"`)
	require.Equal(t, http.StatusOK, getRequest(t.Context(), w, "/v1/result/"+id).Code)
}

func TestWorkerPersistsConfirmed429RefundWhenResponseReadCancelsContext(t *testing.T) {
	o := workerOptions(t)
	normal := o.Client.Transport
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	o.Client = &http.Client{Transport: backendTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				return nil, err
			}
			return &http.Response{
				Request:    r,
				StatusCode: http.StatusTooManyRequests,
				Header:     make(http.Header),
				Body:       cancelingBackendResponseBody{cancel: cancel},
			}, nil
		}
		return normal.RoundTrip(r)
	})}
	w := openWorker(t, o)
	id := taskID(t, submitRequest(t, w, strings.Repeat("a", 64), []byte("synthetic")))
	_, err := w.RunOnce(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, w.Close(context.WithoutCancel(t.Context())))

	reopened, err := Open(t.Context(), o)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close(context.WithoutCancel(t.Context()))) })
	item, ok := reopened.jobs[id]
	require.True(t, ok)
	require.Equal(t, 0, item.Submissions)
	require.Equal(t, "pending", item.State)
}
