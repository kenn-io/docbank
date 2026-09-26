package api_test

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestAgentSessionRoutesMintScopeAndRevokeWithoutMasterFallback(t *testing.T) {
	var allowed, hidden store.Node
	ts, _ := newTestServer(t, func(deps *api.Deps) {
		allowed = createPolicyFile(t, deps, "allowed.txt", "allowed")
		hidden = createPolicyFile(t, deps, "hidden.txt", "hidden")
	})
	emptyRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		ts.URL+"/api/v1/agent-sessions", bytes.NewBufferString(`{"operations":["read"],"source_ids":[],"ttl_seconds":60}`))
	require.NoError(t, err)
	emptyRequest.Header.Set("Content-Type", "application/json")
	emptyResponse, err := ts.Client().Do(emptyRequest)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, emptyResponse.StatusCode)
	require.NoError(t, emptyResponse.Body.Close())
	requestBody, err := json.Marshal(map[string]any{
		"operations": []string{"read"}, "source_ids": []string{allowed.CurrentVersionID},
		"ttl_seconds": 60,
	})
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		ts.URL+"/api/v1/agent-sessions", bytes.NewReader(requestBody))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := ts.Client().Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, response.StatusCode)
	var issued struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	require.NoError(t, json.UnmarshalRead(response.Body, &issued))
	require.NoError(t, response.Body.Close())
	require.NotEmpty(t, issued.ID)
	require.NotEmpty(t, issued.Token)

	read := func(token, path string) int {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
			ts.URL+path, nil)
		require.NoError(t, err)
		request.Header.Set("X-Api-Key", "") // suppress the test client's master key
		request.Header.Set(api.AgentSessionHeader, token)
		response, err := ts.Client().Do(request)
		require.NoError(t, err)
		defer func() { require.NoError(t, response.Body.Close()) }()
		return response.StatusCode
	}
	require.Equal(t, http.StatusOK, read(issued.Token, "/api/v1/capabilities"))
	require.Equal(t, http.StatusOK, read(issued.Token, "/api/v1/versions/"+allowed.CurrentVersionID))
	require.Equal(t, http.StatusNotFound, read(issued.Token, "/api/v1/versions/"+hidden.CurrentVersionID))
	require.Equal(t, http.StatusUnauthorized, read("forged", "/api/v1/capabilities"))

	revoke, err := http.NewRequestWithContext(t.Context(), http.MethodDelete,
		ts.URL+"/api/v1/agent-sessions/"+issued.ID, nil)
	require.NoError(t, err)
	response, err = ts.Client().Do(revoke)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, response.StatusCode)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusUnauthorized, read(issued.Token, "/api/v1/capabilities"))
}
