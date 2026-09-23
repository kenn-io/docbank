package api_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTagConceptRoutesRegisterAndRequireAuth(t *testing.T) {
	ts, catalog := newTestServer(t, nil)
	tag, err := catalog.CreateTag(t.Context(), "Café/blue")
	require.NoError(t, err)
	response, body := do(t, ts, http.MethodGet, "/api/v1/tags/"+tag.ID+"/concept", nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	request, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/tags/"+tag.ID+"/concept", nil)
	require.NoError(t, err)
	request.Header.Set("X-Api-Key", "")
	unauthorized, err := ts.Client().Do(request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = unauthorized.Body.Close() })
	require.Equal(t, http.StatusUnauthorized, unauthorized.StatusCode)
}
