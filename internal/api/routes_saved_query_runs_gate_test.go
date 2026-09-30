package api_test

import (
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

// Definition reads remain available during maintenance, but receipt writes must
// wait until it ends. Gating the whole build incorrectly rejects the read.
func TestSavedQueryRunGatesOnlyReceiptMutation(t *testing.T) {
	t.Parallel()
	gate := api.NewOperationGate()
	ts, s := newTestServer(t, func(d *api.Deps) { d.Gate = gate })
	saved, _ := createSavedQuery(t, ts.URL, "Synthetic maintenance query", `{}`)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	done := make(chan error, 1)
	go func() {
		done <- gate.MaintainContext(t.Context(), func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	headers := map[string]string{"X-Api-Key": testAPIKey, "If-Match": `"1"`}
	resp, body := rawJSONRequest(t, ts.URL, http.MethodPost,
		"/api/v1/saved-queries/11111111-1111-4111-8111-111111111111/runs", headers, `{}`)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, body)
	resp, body = rawJSONRequest(t, ts.URL, http.MethodPost,
		"/api/v1/saved-queries/"+saved.ID+"/runs", headers, `{}`)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, body)
	assert.Equal(t, "maintenance_busy", decodeProblem(t, body).Code)
	runs, err := s.ListSavedQueryRuns(t.Context(), saved.ID, 100)
	require.NoError(t, err)
	assert.Empty(t, runs)
	once.Do(func() { close(release) })
	require.NoError(t, <-done)
	resp, body = rawJSONRequest(t, ts.URL, http.MethodPost,
		"/api/v1/saved-queries/"+saved.ID+"/runs", headers, `{}`)
	assert.Equal(t, http.StatusOK, resp.StatusCode, body)
	runs, err = s.ListSavedQueryRuns(t.Context(), saved.ID, 100)
	require.NoError(t, err)
	assert.Len(t, runs, 1)
}
