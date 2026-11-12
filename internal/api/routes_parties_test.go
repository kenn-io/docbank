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
	resp, body = get(t, ts, "/api/v1/parties?own=perhaps", nil)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	path := "/api/v1/parties/" + party.PartyID
	resp, body = do(t, ts, http.MethodPatch, path, nil, map[string]any{"legal_name": "Northwind Ltd"})
	assert.Equal(t, http.StatusPreconditionRequired, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPost, path+"/retire", nil, nil)
	assert.Equal(t, http.StatusPreconditionRequired, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": "invalid"}, map[string]any{"legal_name": "Northwind Ltd"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"2"`}, map[string]any{"legal_name": "Future"})
	assert.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"1"`}, map[string]any{"legal_name": "Northwind Ltd", "country": nil})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &party))
	assert.Nil(t, party.Country)
	assert.Equal(t, "Northwind Ltd", party.LegalName)
	resp, body = do(t, ts, http.MethodPost, path+"/retire", map[string]string{"If-Match": `"1"`}, nil)
	assert.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPost, path+"/retire", map[string]string{"If-Match": `"2"`}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
}

func TestPartyPatchRejectsNoncanonicalAndNullFields(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	resp, body := do(t, ts, http.MethodPost, "/api/v1/parties", nil, map[string]any{"kind": "person", "display_name": "Example"})
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var party api.Party
	require.NoError(t, json.Unmarshal([]byte(body), &party))
	path := "/api/v1/parties/" + party.PartyID
	for _, patch := range []map[string]any{{}, {"OWN": true}, {"Country": nil}, {"own": nil}, {"display_name": nil}, {"kind": nil}, {"legal_name": nil}} {
		resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"1"`}, patch)
		assert.GreaterOrEqual(t, resp.StatusCode, 400, body)
		assert.Less(t, resp.StatusCode, 500, body)
	}
	resp, body = get(t, ts, path, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &party))
	assert.Equal(t, int64(1), party.Revision)
}

func TestPartyCreateRejectsNoncanonicalAndNullFields(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	for _, field := range []map[string]any{{"OWN": true}, {"Legal_Name": "Example"}, {"Country": "CA"}, {"own": nil}, {"legal_name": nil}} {
		body := map[string]any{"kind": "person", "display_name": "Example"}
		for k, v := range field {
			body[k] = v
		}
		resp, text := do(t, ts, http.MethodPost, "/api/v1/parties", nil, body)
		assert.GreaterOrEqual(t, resp.StatusCode, 400, text)
		assert.Less(t, resp.StatusCode, 500, text)
	}
	resp, body := do(t, ts, http.MethodPost, "/api/v1/parties", nil, map[string]any{"kind": "person", "display_name": "Example"})
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var party api.Party
	require.NoError(t, json.Unmarshal([]byte(body), &party))
	assert.False(t, party.Own)
	assert.Empty(t, party.LegalName)
}
