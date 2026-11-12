package api_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

// The expiry boundary and stale edit must be visible at the HTTP boundary.
func TestContractRoutesExpiryAndRevision(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	resp, body := do(t, ts, http.MethodPost, "/api/v1/contracts", nil, map[string]any{"title": "Agreement", "status": "active", "expires_on": "2024-03-01"})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var c api.Contract
	require.NoError(t, json.Unmarshal([]byte(body), &c))
	resp, body = get(t, ts, "/api/v1/contracts/"+c.ContractID+"?as_of=2024-03-01", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.Unmarshal([]byte(body), &c))
	require.Equal(t, "active", c.EffectiveStatus)
	resp, body = get(t, ts, "/api/v1/contracts/"+c.ContractID+"?as_of=2024-03-02", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.Unmarshal([]byte(body), &c))
	require.Equal(t, "expired", c.EffectiveStatus)
	path := "/api/v1/contracts/" + c.ContractID
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"1"`}, map[string]any{"expires_on": nil})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &c))
	require.Nil(t, c.ExpiresOn)
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"1"`}, map[string]any{"title": "Stale"})
	require.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"2"`}, map[string]any{"title": nil})
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
}
