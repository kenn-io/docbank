package api_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestPartyLifecycleHTTP(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	resp, body := do(t, ts, http.MethodPost, "/api/v1/parties", nil, map[string]any{"kind": "organization", "display_name": "Northwind", "own": true, "country": "CA"})
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var party api.Party
	require.NoError(t, json.Unmarshal([]byte(body), &party))
	assert.Equal(t, `"1"`, resp.Header.Get("ETag"))
	resp, body = do(t, ts, http.MethodPost, "/api/v1/parties", nil, map[string]any{"kind": "organization", "display_name": "NORTHWIND"})
	assert.Equal(t, http.StatusConflict, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"party_exists"`)
	assert.Contains(t, body, party.PartyID)
	path := "/api/v1/parties/" + party.PartyID
	resp, body = do(t, ts, http.MethodPatch, path, nil, map[string]any{"legal_name": "Northwind Ltd"})
	assert.Equal(t, http.StatusPreconditionRequired, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"1"`}, map[string]any{"legal_name": "Northwind Ltd", "country": nil})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &party))
	assert.Nil(t, party.Country)
	assert.Equal(t, "Northwind Ltd", party.LegalName)
	resp, body = do(t, ts, http.MethodPost, path+"/retire", map[string]string{"If-Match": `"1"`}, nil)
	assert.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPost, path+"/retire", map[string]string{"If-Match": `"2"`}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = get(t, ts, "/api/v1/parties?own=true", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page api.PartyPage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	assert.Empty(t, page.Items)
	resp, body = get(t, ts, "/api/v1/parties?include_retired=true", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	assert.Len(t, page.Items, 1)
}
