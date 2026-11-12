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
	resp, body := do(t, ts, http.MethodPost, "/api/v1/contracts", nil, map[string]any{"$schema": "https://example.com/schemas/CreateContractRequest.json", "title": "Agreement", "status": "active", "expires_on": "2024-03-01"})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var c api.Contract
	require.NoError(t, json.Unmarshal([]byte(body), &c))
	resp, body = get(t, ts, "/api/v1/contracts/"+c.ContractID+"?as_of=2024-03-02", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.Unmarshal([]byte(body), &c))
	require.Equal(t, "expired", c.EffectiveStatus)
	path := "/api/v1/contracts/" + c.ContractID
	for _, patch := range []map[string]any{{}, {"$schema": "https://example.com/schemas/PatchContractRequest.json"}} {
		resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"1"`}, patch)
		require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	}
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"1"`}, map[string]any{"$schema": "https://example.com/schemas/PatchContractRequest.json", "expires_on": nil})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &c))
	require.Nil(t, c.ExpiresOn)
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"1"`}, map[string]any{"title": "Stale"})
	require.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"3"`}, map[string]any{"title": "Future"})
	require.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"2"`}, map[string]any{"title": nil})
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodDelete, path, map[string]string{"If-Match": `"1"`}, nil)
	require.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, body)
}

func TestContractCreateRejectsMisnamedAndNullFields(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	for _, body := range []map[string]any{
		{"title": "Agreement", "Status": "active"},
		{"title": "Agreement", "status": nil},
		{"title": "Agreement", "status": ""},
		{"title": "Agreement", "parties": nil},
		{"title": "Agreement", "parties": []any{map[string]any{"party_id": "11111111-1111-4111-8111-111111111111", "role": "own", "Role": "counterparty"}}},
	} {
		resp, response := do(t, ts, http.MethodPost, "/api/v1/contracts", nil, body)
		require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, response)
	}
}

func TestContractPartyAdmissionHTTP(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	resp, body := do(t, ts, http.MethodPost, "/api/v1/parties", nil, map[string]any{"kind": "organization", "display_name": "Supplier"})
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var p api.Party
	require.NoError(t, json.Unmarshal([]byte(body), &p))
	resp, body = do(t, ts, http.MethodPost, "/api/v1/contracts", nil, map[string]any{"title": "Agreement"})
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var c api.Contract
	require.NoError(t, json.Unmarshal([]byte(body), &c))
	path := "/api/v1/contracts/" + c.ContractID + "/parties/" + p.PartyID
	headers := map[string]string{"If-Match": `"1"`}
	resp, body = do(t, ts, http.MethodPut, path+"?role=own", headers, nil)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	require.Contains(t, body, "party_not_own")
	resp, body = do(t, ts, http.MethodPut, path+"?role=counterparty", headers, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPut, path+"?role=counterparty", map[string]string{"If-Match": `"2"`}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodDelete, path+"?role=invalid", map[string]string{"If-Match": `"2"`}, nil)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodDelete, path+"?role=own", map[string]string{"If-Match": `"2"`}, nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/parties/"+p.PartyID+"/retire", headers, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPut, path+"?role=own", map[string]string{"If-Match": `"2"`}, nil)
	require.Equal(t, http.StatusConflict, resp.StatusCode, body)
	require.Contains(t, body, "party_retired")
	resp, body = do(t, ts, http.MethodPut, "/api/v1/contracts/"+c.ContractID+"/parties/11111111-1111-4111-8111-111111111111?role=own", map[string]string{"If-Match": `"2"`}, nil)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	require.Contains(t, body, "invalid_contract")
}
