package api_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewServerRegistersContentMapRoutes(t *testing.T) {
	ts, _ := newTestServer(t, nil)

	resp, body := do(t, ts, http.MethodPost, "/api/v1/maps/plans", nil, map[string]any{})
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodDelete,
		"/api/v1/maps/00000000-0000-4000-8000-000000000001", nil, nil)
	require.Equal(t, http.StatusPreconditionRequired, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"precondition_required"`)

	request, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/maps/plans", nil)
	require.NoError(t, err)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, response.Body.Close()) })
	assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
}
