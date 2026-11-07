package api_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExternalPersonRoutesKeepArchiveScopeAndRevision(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, nil)
	person, err := catalog.CreatePerson(t.Context(), "Synthetic Namesake", "operator")
	require.NoError(t, err)
	other, err := catalog.CreatePerson(t.Context(), "Synthetic Namesake", "operator")
	require.NoError(t, err)
	link := "/api/v1/people/by-id/" + person.PersonID + "/external-identities"
	body := map[string]any{"system": "msgvault", "archive_id": "archive-a", "uid": "stable-card", "uid_state": "current"}
	response, raw := do(t, ts, http.MethodPut, link, map[string]string{"If-Match": `"1"`}, body)
	require.Equal(t, http.StatusOK, response.StatusCode, raw)
	assert.Equal(t, `"2"`, response.Header.Get("ETag"))
	response, raw = do(t, ts, http.MethodPut, link, map[string]string{"If-Match": `"1"`}, body)
	assert.Equal(t, http.StatusPreconditionFailed, response.StatusCode, raw)
	response, raw = do(t, ts, http.MethodPut, "/api/v1/people/by-id/"+other.PersonID+"/external-identities", map[string]string{"If-Match": `"1"`}, body)
	assert.Equal(t, http.StatusConflict, response.StatusCode, raw)
	resolve := "/api/v1/people/resolve-external?system=msgvault&archive_id=archive-a&uid=stable-card"
	response, raw = do(t, ts, http.MethodGet, resolve, nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, raw)
	var resolved map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &resolved))
	assert.Equal(t, person.PersonID, resolved["person_id"])
	assert.Equal(t, "stable-card", resolved["resolved_uid"])
	response, raw = do(t, ts, http.MethodGet, "/api/v1/people/resolve-external?system=msgvault&archive_id=archive-b&uid=stable-card", nil, nil)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, raw)
	body["uid_state"] = "unlinked"
	response, raw = do(t, ts, http.MethodPut, link, map[string]string{"If-Match": `"2"`}, body)
	require.Equal(t, http.StatusOK, response.StatusCode, raw)
	response, raw = do(t, ts, http.MethodGet, resolve, nil, nil)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, raw)
	response, raw = do(t, ts, http.MethodGet, resolve, map[string]string{"X-Api-Key": "", "X-Docbank-Web-Session": issueWebSession(t, ts)}, nil)
	assert.Equal(t, http.StatusForbidden, response.StatusCode, raw)
}
