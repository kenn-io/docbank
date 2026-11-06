package conversionworker

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestWorkerDuplicateBackendIdentityNeverBecomesIssuedHandle(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		// Both duplicate values range over the complete valid backend-ID alphabet
		// and byte lengths. The old parser polled even for first-0/second-0.
		identity := func() string {
			raw := hegel.Draw(ht, hegel.Lists(hegel.SampledFrom([]byte("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_"))))
			if len(raw) == 0 {
				return "a"
			}
			return string(raw[:min(len(raw), 128)])
		}
		first, second := identity(), identity()
		options := workerOptions(t)
		normal := options.Client.Transport
		var polls atomic.Int32
		options.Client.Transport = backendTransport(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodPost {
				_, _ = io.Copy(io.Discard, r.Body)
				return backendResponse(r, http.StatusAccepted, fmt.Sprintf(`{"task_id":%q,"task_id":%q,"task_status":"pending"}`, first, second)), nil
			}
			if strings.HasPrefix(r.URL.Path, "/v1/status/poll/") {
				polls.Add(1)
			}
			return normal.RoundTrip(r)
		})
		worker := openWorker(t, options)
		accepted := submitRequest(t, worker, strings.Repeat("a", 64), []byte("synthetic PDF bytes"))
		require.Equal(ht, http.StatusAccepted, accepted.Code)
		ran, err := worker.RunOnce(t.Context())
		require.NoError(ht, err)
		require.True(ht, ran)
		require.Zero(ht, polls.Load(), "ambiguous JSON must not grant an issued backend identity")
		require.NoError(ht, worker.Close(t.Context()))
	})
}
