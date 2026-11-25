package main

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWorkerDuplicateBackendIdentityNeverBecomesIssuedHandle(t *testing.T) {
	options := workerOptions(t)
	normal := options.Client.Transport
	polls := 0
	options.Client.Transport = backendTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			_, _ = io.Copy(io.Discard, r.Body)
			return backendResponse(r, http.StatusAccepted, `{"task_id":"first","task_id":"second","task_status":"pending","task_type":"convert"}`), nil
		}
		if strings.HasPrefix(r.URL.Path, "/v1/status/poll/") {
			polls++
		}
		return normal.RoundTrip(r)
	})
	worker := openWorker(t, options)
	accepted := submitRequest(t, worker, strings.Repeat("a", 64), []byte("synthetic PDF bytes"))
	require.Equal(t, http.StatusAccepted, accepted.Code)
	ran, err := worker.RunOnce(t.Context())
	require.NoError(t, err)
	require.True(t, ran)
	require.Zero(t, polls, "ambiguous JSON must not grant an issued backend identity")
}
