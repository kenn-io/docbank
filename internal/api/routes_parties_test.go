package api_test

import (
	"encoding/json/v2"
	"maps"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestPartyLifecycleHTTP(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	resp, body := do(t, ts, http.MethodPost, "/api/v1/parties", nil, map[string]any{"$schema": "https://example.com/schemas/CreatePartyRequest.json", "kind": "organization", "display_name": "Northwind", "own": true, "country": "CA"})
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
	resp, body = get(t, ts, "/api/v1/parties?kind=organization&own=true&limit=1&offset=0", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Contains(t, body, party.PartyID)
	resp, body = get(t, ts, "/api/v1/parties?kind=person&own=false", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.NotContains(t, body, party.PartyID)
	path := "/api/v1/parties/" + party.PartyID
	resp, body = do(t, ts, http.MethodPatch, path, nil, map[string]any{"legal_name": "Northwind Ltd"})
	assert.Equal(t, http.StatusPreconditionRequired, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPost, path+"/retire", nil, nil)
	assert.Equal(t, http.StatusPreconditionRequired, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": "invalid"}, map[string]any{"legal_name": "Northwind Ltd"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"2"`}, map[string]any{"legal_name": "Future"})
	assert.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"1"`}, map[string]any{"$schema": "https://example.com/schemas/PatchPartyRequest.json", "legal_name": "Northwind Ltd", "country": nil})
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
	assert.NotContains(t, body, party.PartyID)
	resp, body = get(t, ts, "/api/v1/parties?own=true&include_retired=true", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Contains(t, body, party.PartyID)
}

func TestPartyPatchRejectsNoncanonicalAndNullFields(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	resp, body := do(t, ts, http.MethodPost, "/api/v1/parties", nil, map[string]any{"kind": "person", "display_name": "Example"})
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var party api.Party
	require.NoError(t, json.Unmarshal([]byte(body), &party))
	path := "/api/v1/parties/" + party.PartyID
	for _, patch := range []map[string]any{{}, {"$schema": "https://example.com/schemas/PatchPartyRequest.json"}, {"OWN": true}, {"Country": nil}, {"own": nil}, {"display_name": nil}, {"kind": nil}, {"legal_name": nil}} {
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
		maps.Copy(body, field)
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

func TestPartyPersonLinkAndAuditHTTP(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	survivor, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	absorbed, err := s.CreatePerson(ctx, "Grace", "operator")
	require.NoError(t, err)
	resp, body := do(t, ts, http.MethodPost, "/api/v1/parties", nil, map[string]any{"kind": "person", "display_name": "Grace", "person_id": absorbed.PersonID})
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var party api.Party
	require.NoError(t, json.Unmarshal([]byte(body), &party))
	require.Equal(t, absorbed.PersonID, *party.PersonID)
	_, err = s.MergePersons(ctx, survivor.PersonID, absorbed.PersonID, uuid.NewString(), 1, 1)
	require.NoError(t, err)
	path := "/api/v1/parties/" + party.PartyID
	resp, body = get(t, ts, path, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &party))
	require.Equal(t, survivor.PersonID, *party.PersonID)
	assert.Equal(t, int64(2), party.Revision)
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"2"`}, map[string]any{"person_id": nil})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &party))
	assert.Nil(t, party.PersonID)
	plan, err := s.PreviewInitialAudit(ctx, s.RootID(), "api", nil)
	require.NoError(t, err)
	_, err = s.EnableInitialAudit(ctx, plan)
	require.NoError(t, err)
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"3"`}, map[string]any{"legal_name": "Example"})
	assert.Equal(t, http.StatusConflict, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"audit_mutation_unsupported"`)
}
